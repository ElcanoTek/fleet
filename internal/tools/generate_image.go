package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
)

// generate_image lets the agent produce a PNG/JPEG/WebP from a text prompt
// (and optional reference images) by calling OpenRouter directly with
// modalities=["image","text"]. The result is written under the conversation
// workspace so the UI's <WorkspaceImage> renderer displays it inline when
// the agent references it via `![alt](filename.png)` in its reply.
//
// We bypass fantasy's Stream path here because fantasy v0.19.0's openrouter
// provider does not surface the `images` field on response messages — only
// reasoning details. Going direct over HTTP keeps the tool predictable and
// lets us swap models freely without waiting for a library upgrade.

const (
	// defaultImageGenModel is the model the tool uses when the agent doesn't
	// pass `model` and CHAT_IMAGE_MODEL isn't set. Nano Banana Pro
	// (Gemini 3 Pro Image Preview) is Google's flagship image-generation
	// model — best real-world grounding, supports 2K/4K and aspect-ratio
	// controls, ~$0.14/image as of 2026-05. For cheaper drafts the agent
	// can pass:
	//   google/gemini-3.1-flash-image-preview  (Nano Banana 2, ~$0.04/img)
	//   google/gemini-2.5-flash-image          (Nano Banana, ~$0.04/img)
	//   openai/gpt-5-image-mini                (GPT-5 Image Mini)
	//   openai/gpt-5-image                     (GPT-5 Image, larger)
	defaultImageGenModel       = "google/gemini-3-pro-image-preview"
	defaultImageGenTimeout     = 180 * time.Second
	maxImageGenReferenceImages = 4
	maxImageGenReferenceBytes  = 8 * 1024 * 1024
	openRouterChatCompletions  = "https://openrouter.ai/api/v1/chat/completions"
)

// imageGenMediaTypes maps a file extension (with leading dot, lowercased) to
// the IANA media type used both as wire format and as the fallback type
// recorded for output files.
var imageGenMediaTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// IsImageMIME reports whether mime starts with image/, with a small allow
// list of common types to avoid surfacing exotic formats vision models may
// not actually support.
func IsImageMIME(mime string) bool {
	mime = strings.TrimSpace(strings.ToLower(mime))
	switch mime {
	case "image/png", "image/jpeg", "image/jpg", "image/gif", "image/webp":
		return true
	}
	return false
}

// ImageMIMEFromName returns a best-guess image media type from the file
// name's extension, or "" if it doesn't look like an image.
func ImageMIMEFromName(name string) string {
	if mt, ok := imageGenMediaTypes[strings.ToLower(filepath.Ext(name))]; ok {
		return mt
	}
	return ""
}

// GenerateImageParams are the typed parameters for the generate_image tool.
type GenerateImageParams struct {
	Prompt          string   `json:"prompt" description:"Description of the image to generate. Be concrete: subject, style, composition, colors, aspect."`
	Filename        string   `json:"filename,omitempty" description:"Optional basename for the saved file (NO extension — the tool picks the right one from the model's actual output format). Letters, numbers, dashes, underscores only; path separators are stripped. If omitted, defaults to image-<timestamp>. The tool returns the actual path; reference THAT in your reply."`
	Model           string   `json:"model,omitempty" description:"OpenRouter image-output model slug. Defaults to google/gemini-3-pro-image-preview (Nano Banana Pro). Other supported: google/gemini-3.1-flash-image-preview, google/gemini-2.5-flash-image, openai/gpt-5-image-mini, openai/gpt-5-image."`
	ReferenceImages []string `json:"reference_images,omitempty" description:"Optional paths to existing images to send as input (for editing, restyling, or composition). Up to 4 files; each <= 8 MB."`
}

// imageGenResult is the structured JSON the tool returns to the agent. The
// heavy data is the file on disk; this is the receipt the agent uses to
// compose its reply via `![alt](relative-path)` markdown.
type imageGenResult struct {
	Path        string `json:"path"`
	Filename    string `json:"filename"`
	Bytes       int    `json:"bytes"`
	Model       string `json:"model"`
	MediaType   string `json:"media_type"`
	CommentText string `json:"comment_text,omitempty"`
	CostUSD     any    `json:"cost_usd,omitempty"`
}

const generateImageDescription = "Generates a photorealistic / illustrative image from a text prompt and saves it under your per-conversation workspace. " +
	"You DO NOT pick the file extension — the model decides the output format (Nano Banana Pro returns JPEG; there's no API param to override) and the tool saves with the matching extension. " +
	"Pass an optional `filename` slug (no extension) to control the basename, otherwise it defaults to image-<timestamp>. The tool returns the actual `path` written; reference that exact path in your reply via ![alt](path) — the chat UI rewrites it to a workspace URL and renders the image inline. " +
	"Default model is Google Nano Banana Pro (google/gemini-3-pro-image-preview, ~$0.14/image); cheaper drafts: model=google/gemini-3.1-flash-image-preview (Nano Banana 2) or model=google/gemini-2.5-flash-image (Nano Banana). " +
	"Use `reference_images` to edit / restyle existing images (including ones the user attached this turn — their absolute paths are in the attachments block). " +
	"Do NOT use this for charts, plots, or data visualizations — use run_python with matplotlib instead (free, deterministic, can read your data). Requires OPENROUTER_API_KEY."

// NewGenerateImageTool returns a fantasy.AgentTool that produces an image
// from a prompt and writes it under the conversation workspace.
func NewGenerateImageTool() fantasy.AgentTool {
	client := &http.Client{Timeout: defaultImageGenTimeout}
	return fantasy.NewAgentTool("generate_image", generateImageDescription,
		func(ctx context.Context, params GenerateImageParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, err := runGenerateImage(ctx, client, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			payload, mErr := json.Marshal(result)
			if mErr != nil {
				return fantasy.NewTextErrorResponse(mErr.Error()), nil
			}
			return fantasy.NewTextResponse(string(payload)), nil
		})
}

func runGenerateImage(ctx context.Context, client *http.Client, params GenerateImageParams) (*imageGenResult, error) {
	if strings.TrimSpace(params.Prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		return nil, errors.New("OPENROUTER_API_KEY is not set")
	}

	model := strings.TrimSpace(params.Model)
	if model == "" {
		if env := strings.TrimSpace(fleetEnv("IMAGE_MODEL")); env != "" {
			model = env
		} else {
			model = defaultImageGenModel
		}
	}

	if len(params.ReferenceImages) > maxImageGenReferenceImages {
		return nil, fmt.Errorf("reference_images exceeds limit of %d", maxImageGenReferenceImages)
	}

	userContent := []map[string]any{
		{"type": "text", "text": params.Prompt},
	}
	for _, ref := range params.ReferenceImages {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			continue
		}
		validRef, err := ValidatePathForRead(resolveWorkspacePath(ctx, ref))
		if err != nil {
			return nil, fmt.Errorf("reference image %q: %w", ref, err)
		}
		info, err := os.Stat(validRef)
		if err != nil {
			return nil, fmt.Errorf("reference image %q: %w", ref, err)
		}
		if info.Size() > maxImageGenReferenceBytes {
			return nil, fmt.Errorf("reference image %q exceeds %d bytes", ref, maxImageGenReferenceBytes)
		}
		data, err := os.ReadFile(validRef) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("read reference image %q: %w", ref, err)
		}
		mt := ImageMIMEFromName(validRef)
		if mt == "" {
			mt = "image/png"
		}
		dataURI := "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
		userContent = append(userContent, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURI},
		})
	}

	body := map[string]any{
		"model":      model,
		"modalities": []string{"image", "text"},
		"messages": []map[string]any{
			{"role": "user", "content": userContent},
		},
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openRouterChatCompletions, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	if referer := strings.TrimSpace(os.Getenv("OPENROUTER_HTTP_REFERER")); referer != "" {
		req.Header.Set("HTTP-Referer", referer)
	}
	if title := strings.TrimSpace(os.Getenv("OPENROUTER_X_TITLE")); title != "" {
		req.Header.Set("X-Title", title)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openrouter request: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 32*1024*1024))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		preview := string(respBytes)
		if len(preview) > 1024 {
			preview = preview[:1024] + "…"
		}
		return nil, fmt.Errorf("openrouter HTTP %d: %s", resp.StatusCode, preview)
	}

	mediaType, data, comment, cost, err := parseImageGenResponse(respBytes)
	if err != nil {
		return nil, err
	}

	// The tool — not the agent — picks the filename and extension. Reasons:
	// (1) the model decides the output format and there's no API param to
	// override it, so the agent has no way to know what extension to ask
	// for; (2) keeping naming inside the tool makes the returned path
	// authoritative for the agent's markdown reference, removing a class
	// of "wrong filename" bugs.
	ext := canonicalExtForImageMedia(mediaType)
	if ext == "" {
		ext = ".bin" // unknown media type; bytes still preserved
	}
	filename := sanitizeImageFilename(params.Filename) + ext
	resolved := resolveWorkspacePath(ctx, filename)
	validOut, err := ValidatePath(resolved)
	if err != nil {
		return nil, fmt.Errorf("output path validation failed: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(validOut), 0o755); err != nil { //nolint:gosec // workspace dir must be readable by the sandbox user
		return nil, fmt.Errorf("mkdir output: %w", err)
	}
	if err := os.WriteFile(validOut, data, 0o644); err != nil { //nolint:gosec // workspace files are readable by the same user
		return nil, fmt.Errorf("write output: %w", err)
	}

	return &imageGenResult{
		Path:        validOut,
		Filename:    filepath.Base(validOut),
		Bytes:       len(data),
		Model:       model,
		MediaType:   mediaType,
		CommentText: comment,
		CostUSD:     cost,
	}, nil
}

// canonicalExtForImageMedia maps an IANA image media type to the file
// extension the tool uses on disk. Returns "" for unknown types so callers
// can fall back to .bin and still preserve the bytes.
func canonicalExtForImageMedia(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ""
	}
}

// sanitizeImageFilename strips path separators, leading dots, and any
// trailing recognized image extension; falls back to a timestamped slug
// when nothing usable remains. We intentionally do NOT preserve the agent's
// extension (the tool picks one based on the model's actual output format).
func sanitizeImageFilename(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return defaultImageFilename()
	}
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	if ext := strings.ToLower(filepath.Ext(s)); ext != "" {
		if _, ok := imageGenMediaTypes[ext]; ok {
			s = strings.TrimSuffix(s, filepath.Ext(s))
		}
	}
	s = strings.TrimLeft(s, ".")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return defaultImageFilename()
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func defaultImageFilename() string {
	return fmt.Sprintf("image-%d", time.Now().UnixNano())
}

// parseImageGenResponse extracts the first generated image from an OpenRouter
// chat-completion response. OpenRouter image-output models return data: URIs
// in choices[0].message.images[].image_url.url; if none is present, the
// model probably refused or returned plain text — we surface that text as the
// error so the agent can react.
func parseImageGenResponse(body []byte) (mediaType string, data []byte, comment string, cost any, err error) {
	var raw struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Images  []struct {
					ImageURL struct {
						URL string `json:"url"`
					} `json:"image_url"`
				} `json:"images"`
			} `json:"message"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", nil, "", nil, fmt.Errorf("parse response: %w", err)
	}
	if len(raw.Choices) == 0 {
		return "", nil, "", nil, errors.New("openrouter response had no choices")
	}
	msg := raw.Choices[0].Message
	if len(msg.Images) == 0 || strings.TrimSpace(msg.Images[0].ImageURL.URL) == "" {
		text := strings.TrimSpace(msg.Content)
		if text == "" {
			text = "model returned no image and no text"
		}
		return "", nil, "", nil, fmt.Errorf("no image in response (model said: %s)", truncateImageGenError(text, 400))
	}
	mt, payload, decErr := decodeImageDataURI(msg.Images[0].ImageURL.URL)
	if decErr != nil {
		return "", nil, "", nil, decErr
	}
	var c any
	if raw.Usage != nil {
		c = raw.Usage["cost"]
	}
	return mt, payload, strings.TrimSpace(msg.Content), c, nil
}

// decodeImageDataURI accepts data:<media>;base64,<payload>. Anything else
// (e.g. https://) is rejected so the tool can't be coerced into fetching
// arbitrary URLs and writing them to disk.
func decodeImageDataURI(uri string) (string, []byte, error) {
	if !strings.HasPrefix(uri, "data:") {
		return "", nil, fmt.Errorf("unsupported image url scheme (only data: URIs are accepted)")
	}
	rest := strings.TrimPrefix(uri, "data:")
	semi := strings.Index(rest, ",")
	if semi < 0 {
		return "", nil, errors.New("malformed data URI (missing comma)")
	}
	header := rest[:semi]
	payload := rest[semi+1:]
	parts := strings.Split(header, ";")
	media := parts[0]
	isBase64 := false
	for _, p := range parts[1:] {
		if strings.EqualFold(p, "base64") {
			isBase64 = true
		}
	}
	if !isBase64 {
		return "", nil, fmt.Errorf("only base64-encoded data URIs are supported")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if d2, e2 := base64.RawStdEncoding.DecodeString(payload); e2 == nil {
			decoded = d2
		} else {
			return "", nil, fmt.Errorf("decode base64: %w", err)
		}
	}
	return media, decoded, nil
}

func truncateImageGenError(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
