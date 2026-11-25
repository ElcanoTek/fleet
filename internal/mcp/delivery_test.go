package mcp

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
)

// A stdio server that dies after reading the tool request, and whose restart
// then fails writing its own initialize, produces a text that contains
// "request not delivered" — the restart's. The tool request itself may have
// run, so the error must not say it was not sent.
func TestRestartFailedError_DeliveryComesFromTheToolRequest(t *testing.T) {
	restartErr := fmt.Errorf("failed to reinitialize server: %w",
		fmt.Errorf("initialize call failed: %w", fmt.Errorf("%s: %w", errStdioWriteFailed, syscall.EPIPE)))

	readSideDeath := restartFailedError(io.EOF, restartErr)
	if !strings.Contains(readSideDeath.Error(), "original=EOF, restart=failed to reinitialize server: initialize call failed: stdio write failed (request not delivered)") {
		t.Fatalf("unexpected text %q", readSideDeath)
	}
	if RequestNotSent(readSideDeath) {
		t.Fatal("a tool request read by the dead server reported as not sent because the restart's initialize was not delivered")
	}

	writeFailure := restartFailedError(MarkNotSent(fmt.Errorf("%s: %w", errStdioWriteFailed, syscall.EPIPE)), restartErr)
	if !RequestNotSent(writeFailure) {
		t.Fatal("a tool request whose own write failed must report not sent")
	}
}

func TestRequestNotSent(t *testing.T) {
	base := errors.New("boom")
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"unmarked", base, false},
		{"marked", MarkNotSent(base), true},
		{"marked then wrapped", fmt.Errorf("context: %w", MarkNotSent(base)), true},
		{"outer may-have-been-sent overrules inner not-sent", markDelivery(fmt.Errorf("x: %w", MarkNotSent(base)), false), false},
		{"stopped before send", ErrStoppedBeforeSend, true},
		{"stopped before send, wrapped", fmt.Errorf("x: %w", ErrStoppedBeforeSend), true},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := RequestNotSent(tc.err); got != tc.want {
			t.Errorf("%s: RequestNotSent = %v, want %v", tc.name, got, tc.want)
		}
	}
	if MarkNotSent(nil) != nil {
		t.Error("MarkNotSent(nil) must stay nil")
	}
}

// The plain transport branch of callTool keeps a stop-before-send's answer:
// ErrStoppedBeforeSend carries no "request not delivered" text, and marking
// it "may have been sent" would overrule it.
func TestCallNotSent_StoppedBeforeSend(t *testing.T) {
	if !RequestNotSent(markDelivery(ErrStoppedBeforeSend, callNotSent(ErrStoppedBeforeSend))) {
		t.Fatal("stop before send reported as possibly sent")
	}
}
