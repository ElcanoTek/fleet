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
	"net/http"
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

func NewHTTPDetector(url string) (*HTTPDetector, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return nil, errors.New("guardrail detector URL is required")
	}
	return &HTTPDetector{url: url, client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func (d *HTTPDetector) Check(ctx context.Context, profile, source, text string) (Verdict, error) {
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
