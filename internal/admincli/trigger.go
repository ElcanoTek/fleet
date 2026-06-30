package admincli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// slugPattern matches a URL-safe trigger slug: a lowercase alphanumeric start
// then up to 127 more of [a-z0-9_-]. Mirrors the schema comment in
// migration 021.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,127}$`)

// cmdSchedTrigger dispatches `fleet-admin sched trigger create|list|delete|rotate`.
func cmdSchedTrigger(argv []string) int {
	if len(argv) < 1 {
		return errf(1, "usage: fleet-admin sched trigger create|list|delete|rotate")
	}
	switch argv[0] {
	case "create":
		return schedTriggerCreate(argv[1:])
	case "list", "ls":
		return schedTriggerList(argv[1:])
	case "delete", "del", "rm":
		return schedTriggerDelete(argv[1:])
	case "rotate":
		return schedTriggerRotate(argv[1:])
	default:
		return errf(1, "unknown sched trigger subcommand %q (want create|list|delete|rotate)", argv[0])
	}
}

// generateTriggerSecret returns a fresh 32-byte HMAC secret as hex.
func generateTriggerSecret() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func schedTriggerCreate(argv []string) int {
	fs := flag.NewFlagSet("sched trigger create", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	taskID := fs.String("task", "", "template task ID the trigger fires (required)")
	slug := fs.String("slug", "", "URL-safe slug, [a-z0-9][a-z0-9_-]{0,127} (required)")
	secret := fs.String("secret", "", "HMAC-SHA256 secret (hex/base64); generated if omitted")
	templateFile := fs.String("template", "", "path to a Go text/template prompt file (optional)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	tid, err := uuid.Parse(strings.TrimSpace(*taskID))
	if err != nil {
		return errf(1, "--task must be a valid task UUID: %v", err)
	}
	if !slugPattern.MatchString(*slug) {
		return errf(1, "--slug must match [a-z0-9][a-z0-9_-]{0,127}")
	}

	promptTemplate := ""
	if strings.TrimSpace(*templateFile) != "" {
		data, rerr := os.ReadFile(*templateFile)
		if rerr != nil {
			return errf(1, "read --template file: %v", rerr)
		}
		promptTemplate = string(data)
	}

	sec := strings.TrimSpace(*secret)
	generated := false
	if sec == "" {
		sec, err = generateTriggerSecret()
		if err != nil {
			return errf(5, "%v", err)
		}
		generated = true
	}
	if len(sec) < 32 {
		return errf(1, "--secret must be at least 32 characters")
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	ctx := context.Background()

	task, err := st.GetTask(tid)
	if err != nil || task == nil {
		return errf(2, "task %s not found", tid)
	}
	if task.TriggerType != models.TriggerTypeWebhook {
		fmt.Fprintf(os.Stderr, "note: task %s is trigger_type=%q, not %q — it may also run on its own schedule. "+
			"Create the task with trigger_type=webhook to make it a pure webhook template.\n",
			tid, task.TriggerType, models.TriggerTypeWebhook)
	}

	trig := &models.TaskTrigger{
		ID:             uuid.New(),
		TaskID:         tid,
		Slug:           *slug,
		Secret:         sec,
		PromptTemplate: promptTemplate,
	}
	if err := st.CreateTrigger(ctx, trig); err != nil {
		return errf(5, "create trigger: %v", err)
	}

	fmt.Printf("created trigger %s (slug=%s task=%s)\n", trig.ID, trig.Slug, trig.TaskID)
	fmt.Printf("POST /triggers/%s\n", trig.Slug)
	if generated {
		fmt.Printf("secret (shown once): %s\n", sec)
	}
	return 0
}

func schedTriggerList(argv []string) int {
	fs := flag.NewFlagSet("sched trigger list", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	taskID := fs.String("task", "", "filter to one task ID (optional)")
	if err := fs.Parse(argv); err != nil {
		return 1
	}

	var filter *uuid.UUID
	if strings.TrimSpace(*taskID) != "" {
		tid, err := uuid.Parse(strings.TrimSpace(*taskID))
		if err != nil {
			return errf(1, "--task must be a valid task UUID: %v", err)
		}
		filter = &tid
	}

	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()

	triggers, err := st.ListTriggers(context.Background(), filter)
	if err != nil {
		return errf(5, "list triggers: %v", err)
	}
	if len(triggers) == 0 {
		fmt.Fprintln(os.Stderr, "(no triggers)")
		return 0
	}
	// Secrets are deliberately NOT printed.
	for _, t := range triggers {
		fmt.Printf("%s\t%s\t%s\n", t.ID, t.Slug, t.TaskID)
	}
	return 0
}

func schedTriggerDelete(argv []string) int {
	id, flagArgs := splitPositional(argv)
	fs := flag.NewFlagSet("sched trigger delete", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	tid, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return errf(1, "trigger id required (a UUID): %v", err)
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	deleted, err := st.DeleteTrigger(context.Background(), tid)
	if err != nil {
		return errf(5, "delete trigger: %v", err)
	}
	if !deleted {
		return errf(2, "trigger %s not found", tid)
	}
	fmt.Printf("deleted trigger %s\n", tid)
	return 0
}

func schedTriggerRotate(argv []string) int {
	id, flagArgs := splitPositional(argv)
	fs := flag.NewFlagSet("sched trigger rotate", flag.ContinueOnError)
	dbURL := fs.String("database-url", "", "sched Postgres DSN")
	if err := fs.Parse(flagArgs); err != nil {
		return 1
	}
	tid, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return errf(1, "trigger id required (a UUID): %v", err)
	}
	sec, err := generateTriggerSecret()
	if err != nil {
		return errf(5, "%v", err)
	}
	st, code := openSchedStorage(*dbURL)
	if st == nil {
		return code
	}
	defer st.Close()
	ok, err := st.RotateTriggerSecret(context.Background(), tid, sec)
	if err != nil {
		return errf(5, "rotate trigger: %v", err)
	}
	if !ok {
		return errf(2, "trigger %s not found", tid)
	}
	fmt.Printf("rotated secret for trigger %s\n", tid)
	fmt.Printf("new secret (shown once): %s\n", sec)
	fmt.Fprintln(os.Stderr, "update the external caller's webhook config before the old secret stops working (it already has)")
	return 0
}
