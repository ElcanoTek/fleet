// Package guardrail provides host-enforced screening of untrusted text before
// it enters an LLM context. The detector is deliberately out of process: Fleet
// owns policy and fail behavior while operators choose the local classifier.
package guardrail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Mode string

const (
	ModeOff     Mode = "off"
	ModeObserve Mode = "observe"
	ModeBlock   Mode = "block"
)

func ParseMode(raw string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(raw))) {
	case ModeOff:
		return ModeOff, nil
	case ModeObserve:
		return ModeObserve, nil
	case ModeBlock:
		return ModeBlock, nil
	default:
		return "", fmt.Errorf("guardrail mode must be off, observe, or block")
	}
}

type Verdict struct {
	Flagged bool    `json:"flagged"`
	Score   float64 `json:"score,omitempty"`
	Reason  string  `json:"reason,omitempty"`
}

type Detector interface {
	Check(ctx context.Context, profile, source, text string) (Verdict, error)
}

type UnavailableDetector struct{ Err error }

func (d UnavailableDetector) Check(context.Context, string, string, string) (Verdict, error) {
	if d.Err != nil {
		return Verdict{}, d.Err
	}
	return Verdict{}, errors.New("guardrail detector unavailable")
}

type HTTPDetector struct {
	url    string
	client *http.Client
}

// MaxDetectorBody caps the text a single Check will ship to the detector. The
// caller (agentcore) samples tool output well below this, so hitting it means a
// caller forgot to bound its input; failing loudly here is better than a 5 s
// timeout that surfaces as a spurious detector_error / block.
const MaxDetectorBody = 1 << 20

// ErrTextTooLarge is returned when the text exceeds MaxDetectorBody.
var ErrTextTooLarge = errors.New("guardrail: text exceeds detector body limit")

// NewHTTPDetector validates the detector endpoint. The request body is the raw
// untrusted text being screened (user messages, tool output), so a plaintext
// hop is only acceptable to a loopback classifier; any other host must be
// https so a network-adjacent party cannot read or rewrite verdicts.
func NewHTTPDetector(rawURL string) (*HTTPDetector, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, errors.New("guardrail detector URL is required")
	}
	if err := validateDetectorURL(rawURL); err != nil {
		return nil, err
	}
	return &HTTPDetector{url: rawURL, client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func validateDetectorURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("guardrail detector URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if strings.EqualFold(host, "localhost") {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("guardrail detector URL %q: a non-loopback detector must use https", raw)
	default:
		return fmt.Errorf("guardrail detector URL %q: scheme must be http (loopback only) or https", raw)
	}
}

func (d *HTTPDetector) Check(ctx context.Context, profile, source, text string) (Verdict, error) {
	if len(text) > MaxDetectorBody {
		return Verdict{}, fmt.Errorf("%w: %d bytes > %d", ErrTextTooLarge, len(text), MaxDetectorBody)
	}
	body, err := json.Marshal(map[string]string{"profile": profile, "source": source, "text": text})
	if err != nil {
		return Verdict{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(body))
	if err != nil {
		return Verdict{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return Verdict{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return Verdict{}, fmt.Errorf("guardrail detector returned HTTP %d", resp.StatusCode)
	}
	var out Verdict
	dec := json.NewDecoder(io.LimitReader(resp.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return Verdict{}, fmt.Errorf("decode guardrail verdict: %w", err)
	}
	return out, nil
}
