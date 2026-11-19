package models

import "testing"

// Every reader of a prompt's EXECUTION REQUIREMENTS reads it as it is
// stored, without U+0000, even when given the prompt before it was cleaned:
// a 0x00 byte after the marker hides it from no reader, so NewTask takes the
// serialization key the stored prompt declares.
func TestExecutionRequirementsAreReadAsStored(t *testing.T) {
	t.Parallel()
	prompt := "Refresh the home page.\nEXECUTION REQUIREMENTS (JSON):\x00\n{\"serialization_key\":\"pages:home\"}"
	req, err := ParseExecutionRequirements(prompt)
	if err != nil || req == nil || req.SerializationKey == nil || *req.SerializationKey != "pages:home" {
		t.Fatalf("ParseExecutionRequirements = %+v, %v; want the declaration with key pages:home", req, err)
	}
	if got := DeclaredSerializationKey(prompt); got != "pages:home" {
		t.Fatalf("DeclaredSerializationKey = %q, want pages:home", got)
	}
	task := NewTask(TaskCreate{Prompt: prompt})
	if task.SerializationKey == nil || *task.SerializationKey != "pages:home" {
		t.Fatalf("NewTask serialization key = %v, want pages:home", task.SerializationKey)
	}
	if err := ValidateExecutionRequirements("Refresh.\nEXECUTION REQUIREMENTS (JSON):\x00\n{not json"); err == nil {
		t.Fatal("a malformed declaration hidden by a 0x00 byte validated")
	}
}
