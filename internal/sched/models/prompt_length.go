package models

import "fmt"

// TaskPromptMinLength and TaskPromptMaxLength bound a task prompt in BYTES
// (len(), not runes). The API create and edit handlers enforce them
// (handlers.taskPromptMinLength / taskPromptMaxLength alias these), and so does
// the chat manage_tasks adapter, whose approved prompt edits reach
// storage.UpdateEditableTask without passing through a handler.
const (
	TaskPromptMinLength = 3
	TaskPromptMaxLength = 250000
)

// ValidateTaskPromptLength reports a prompt outside the task prompt bounds,
// with the same wording the API handlers use.
func ValidateTaskPromptLength(prompt string) error {
	if len(prompt) < TaskPromptMinLength {
		return fmt.Errorf("prompt must be at least %d characters", TaskPromptMinLength)
	}
	if len(prompt) > TaskPromptMaxLength {
		return fmt.Errorf("prompt cannot exceed %d bytes", TaskPromptMaxLength)
	}
	return nil
}
