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
			if err != nil || blocked {
				return err
			}
		}
	}
	return nil
}

func screenToolOutput(ctx context.Context, toolName, text string) (string, bool) {
	blocked, err := screenText(ctx, "tool_output", text)
	if err != nil || blocked {
		log.Printf("guardrail: withheld output from %s", toolName)
		return "[BLOCKED: workspace content guardrail]", true
	}
	return text, false
}
