// Conversation labels (#258).
//
// Labels are per-conversation organization metadata stored on the conversations
// row (labels TEXT[], added by #279). This file holds the shared HTTP-layer
// validation for the label field a bulk PATCH supplies; `GET /conversations?label=`
// filtering lives in server.go's list handler.
//
// The sibling `folder` bucket that shipped alongside labels was removed once
// projects (#509) superseded it — see docs/PROJECTS.md.

package httpapi

import (
	"fmt"
	"strings"
)

// Label bounds (#258), enforced at the HTTP layer before any store write.
const (
	maxLabelsPerConversation = 10
	maxLabelLen              = 32
)

// normalizeAndValidateLabels trims and bounds the optional label mutation carried
// by a conversation PATCH (#258). labels, when non-nil, is the full replacement
// set (a non-nil empty slice clears all labels). It mutates the entries in place
// so the store persists trimmed values, and returns a user-facing error on
// violation.
func normalizeAndValidateLabels(labels []string) error {
	if len(labels) > maxLabelsPerConversation {
		return fmt.Errorf("at most %d labels per conversation", maxLabelsPerConversation)
	}
	for i, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			return fmt.Errorf("labels must be non-empty")
		}
		if len(l) > maxLabelLen {
			return fmt.Errorf("each label must be at most %d characters", maxLabelLen)
		}
		labels[i] = l
	}
	return nil
}
