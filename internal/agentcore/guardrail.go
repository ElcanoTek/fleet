package agentcore

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/guardrail"
	"github.com/ElcanoTek/fleet/internal/metrics"
)

var ErrGuardrailBlocked = errors.New("content blocked by workspace guardrail")

type guardrailPolicy struct {
	mode     string
	block    bool
	profile  string
	detector guardrail.Detector
}

var (
	guardrailMu      sync.RWMutex
	processGuardrail *guardrailPolicy
)

func SetGuardrail(enabled, block bool, mode, profile string, detector guardrail.Detector) {
	guardrailMu.Lock()
	defer guardrailMu.Unlock()
	if !enabled || detector == nil {
		processGuardrail = nil
		return
	}
	processGuardrail = &guardrailPolicy{mode: mode, block: block, profile: profile, detector: detector}
}

func currentGuardrail() *guardrailPolicy {
	guardrailMu.RLock()
	defer guardrailMu.RUnlock()
	return processGuardrail
}

func screenText(ctx context.Context, source, text string) (bool, error) {
	p := currentGuardrail()
	if p == nil || text == "" {
		return false, nil
	}
	v, err := p.detector.Check(ctx, p.profile, source, text)
	if err != nil {
		metrics.RecordGuardrailVerdict(source, p.mode, "detector_error")
		log.Printf("guardrail: detector unavailable (source=%s mode=%s): %v", source, p.mode, err)
		if p.block {
			return true, fmt.Errorf("%w: detector unavailable", ErrGuardrailBlocked)
		}
		return false, nil
	}
	if !v.Flagged {
		metrics.RecordGuardrailVerdict(source, p.mode, "clean")
		return false, nil
	}
	log.Printf("guardrail: content flagged (source=%s profile=%s mode=%s score=%.3f)", source, p.profile, p.mode, v.Score)
	metrics.RecordGuardrailVerdict(source, p.mode, "flagged")
	if p.block {
		return true, fmt.Errorf("%w: profile %s", ErrGuardrailBlocked, p.profile)
	}
	return false, nil
}

func screenSeedMessages(ctx context.Context, messages []fantasy.Message) error {
	for _, message := range messages {
		if message.Role != fantasy.MessageRoleUser {
			continue
		}
		for _, part := range message.Content {
			text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
			if !ok {
				continue
			}
			blocked, err := screenText(ctx, "user_message", text.Text)
			if err := seedScreenError(blocked, err); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxGuardrailScreenBytes bounds the text handed to the out-of-process
// detector. Tool output is governed BEFORE the model-output cap
// (governToolOutput → boundModelToolOutput), so without this bound a multi-MB
// bash log is marshalled whole into a 5 s HTTP call; the resulting timeout is a
// detector_error, which in block mode replaces perfectly benign output with the
// [BLOCKED] marker. The screened sample keeps the head and tail (where an
// injection payload is most likely to be placed to survive the model cap) and
// marks the elision; the tool result itself is not altered here.
const maxGuardrailScreenBytes = 256 << 10

// guardrailScreenSample returns the bounded head+tail sample of text that is
// sent to the detector.
func guardrailScreenSample(text string) string {
	return headTailPreview(text, maxGuardrailScreenBytes)
}

// seedScreenError maps a screenText outcome to the error screenSeedMessages
// returns. screenText pairs blocked with a wrapped ErrGuardrailBlocked today,
// but the block must be structural: a (blocked=true, err=nil) shape from any
// future detector path must never degrade into "seed accepted", so it fails
// closed here rather than relying on the pairing.
func seedScreenError(blocked bool, err error) error {
	if err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("%w: user_message", ErrGuardrailBlocked)
	}
	return nil
}

func screenToolOutput(ctx context.Context, toolName, text string) (string, bool) {
	blocked, err := screenText(ctx, "tool_output", guardrailScreenSample(text))
	if err != nil || blocked {
		log.Printf("guardrail: withheld output from %s", toolName)
		return "[BLOCKED: workspace content guardrail]", true
	}
	return text, false
}
