package chattui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// lingeringTurn is a server that streams frames, then holds the stream open
// until release closes (a new conversation being titled), then sends after
// and ends it.
func lingeringTurn(frames []string, after string, release <-chan struct{}) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			fmt.Fprint(w, f)
		}
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, after)
	}))
}

const (
	frameConversation = "event: conversation\ndata: {\"id\":\"conv-t\"}\n\n"
	frameText         = "event: text.delta\ndata: {\"text\":\"hi\"}\n\n"
	frameCompleted    = "event: turn.completed\ndata: {}\n\n"
	frameTitle        = "event: conversation.title_updated\ndata: {\"id\":\"conv-t\",\"title\":\"Hi\"}\n\n"
)

func closed(c <-chan struct{}) bool {
	select {
	case <-c:
		return true
	default:
		return false
	}
}

// StreamTurn answers at the turn's terminal frame, with the outcome
// StreamInput gives, while the server still holds the stream open; it reads
// the rest without passing it on, and rest closes when the stream ends.
func TestStreamTurnReturnsAtTheTerminalFrame(t *testing.T) {
	release := make(chan struct{})
	srv := lingeringTurn([]string{frameConversation, frameText, frameCompleted}, frameTitle, release)
	defer srv.Close()
	var seen []string
	type res struct {
		id   string
		rest <-chan struct{}
		err  error
	}
	got := make(chan res, 1)
	go func() {
		id, rest, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamTurn(context.Background(), "hi", "", "", func(ev Event) {
			seen = append(seen, ev.Name)
		})
		got <- res{id, rest, err}
	}()
	var r res
	select {
	case r = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("StreamTurn waited for the stream to end")
	}
	if r.err != nil || r.id != "conv-t" {
		t.Fatalf("got %q, %v; want conv-t, no error", r.id, r.err)
	}
	if strings.Join(seen, ",") != "conversation,text.delta,turn.completed" {
		t.Errorf("events = %v", seen)
	}
	if closed(r.rest) {
		t.Fatal("rest closed while the server still held the stream open")
	}
	close(release)
	select {
	case <-r.rest:
	case <-time.After(10 * time.Second):
		t.Fatal("rest did not close when the stream ended")
	}
	if strings.Join(seen, ",") != "conversation,text.delta,turn.completed" {
		t.Errorf("events after the terminal frame were passed on: %v", seen)
	}
}

// Every terminal frame ends the call with StreamInput's outcome for it.
func TestStreamTurnOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, frame string
		want        func(error) bool
	}{
		{"completed", frameCompleted, func(err error) bool { return err == nil }},
		{"cancelled", "event: turn.cancelled\ndata: {}\n\n", func(err error) bool { return errors.Is(err, context.Canceled) }},
		{"error", "event: turn.error\ndata: {\"message\":\"boom\"}\n\n", func(err error) bool { return err != nil && err.Error() == "turn failed: boom" }},
		{"model required", "event: turn.model_required\ndata: {\"message\":\"pick another\"}\n\n", func(err error) bool {
			return err != nil && err.Error() == "turn requires another model: pick another"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			srv := lingeringTurn([]string{frameConversation, tc.frame}, "", release)
			defer srv.Close()
			defer close(release) // first: Close waits for the handler
			_, rest, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamTurn(context.Background(), "hi", "", "", func(Event) {})
			if !tc.want(err) {
				t.Errorf("err = %v", err)
			}
			if closed(rest) {
				t.Error("rest closed while the server still held the stream open")
			}
		})
	}
}

// Without a terminal frame there is nothing to answer early: the call ends
// with the stream, as StreamInput's does, and rest is already closed.
func TestStreamTurnWithoutATerminalFrame(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, frameConversation, frameText)
	}))
	defer srv.Close()
	_, rest, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamTurn(context.Background(), "hi", "", "", func(Event) {})
	if err == nil || err.Error() != "stream ended before a terminal turn event" {
		t.Errorf("err = %v", err)
	}
	if !closed(rest) {
		t.Error("rest is open after the stream ended")
	}
}

// A queue acknowledgement has no stream: rest is already closed.
func TestStreamTurnQueuedHasNoRest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprint(w, `{"queued":true,"conversation_id":"c","input":{"id":"i","position":1,"state":"queued"}}`)
	}))
	defer srv.Close()
	_, rest, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamTurn(context.Background(), "hi", "c", "", func(Event) {})
	var q *QueuedError
	if !errors.As(err, &q) {
		t.Fatalf("err = %v, want *QueuedError", err)
	}
	if !closed(rest) {
		t.Error("rest is open for a queued message")
	}
}

// Cancelling ctx ends the read of the rest at once.
func TestStreamTurnRestEndsWithItsContext(t *testing.T) {
	release := make(chan struct{})
	srv := lingeringTurn([]string{frameConversation, frameCompleted}, frameTitle, release)
	defer srv.Close()
	defer close(release) // first: Close waits for the handler
	ctx, cancel := context.WithCancel(context.Background())
	_, rest, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamTurn(ctx, "hi", "", "", func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-rest:
	case <-time.After(10 * time.Second):
		t.Fatal("rest did not close when its context was cancelled")
	}
}

// StreamInput still reads the stream to its end and passes every frame on,
// the title included: `fleet chat` is unchanged.
func TestStreamInputStillReadsToTheEnd(t *testing.T) {
	release := make(chan struct{})
	srv := lingeringTurn([]string{frameConversation, frameCompleted}, frameTitle, release)
	defer srv.Close()
	var seen []string
	done := make(chan error, 1)
	go func() {
		_, err := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"}).StreamInput(context.Background(), "hi", "", "", func(ev Event) {
			seen = append(seen, ev.Name)
		})
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("StreamInput returned before the stream ended")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != "conversation,turn.completed,conversation.title_updated" {
		t.Errorf("events = %v", seen)
	}
}

// A second terminal frame after the first (the server reports a failure of
// its post-turn work after the turn completed): StreamTurn reports the
// first, the turn's own outcome; StreamInput the last.
func TestStreamTurnReportsTheFirstTerminalFrame(t *testing.T) {
	frames := []string{frameConversation, frameCompleted, "event: turn.error\ndata: {\"message\":\"post-turn work failed\"}\n\n"}
	release := make(chan struct{})
	close(release)
	srv := lingeringTurn(frames, "", release)
	defer srv.Close()
	c := NewClient(Config{ServerURL: srv.URL, Email: "a@b.c", Token: "tok"})
	_, rest, err := c.StreamTurn(context.Background(), "hi", "", "", func(Event) {})
	if err != nil {
		t.Errorf("StreamTurn: %v, want the completed turn", err)
	}
	<-rest
	if _, err := c.StreamInput(context.Background(), "hi", "", "", func(Event) {}); err == nil || err.Error() != "turn failed: post-turn work failed" {
		t.Errorf("StreamInput: %v, want the last frame's error", err)
	}
}
