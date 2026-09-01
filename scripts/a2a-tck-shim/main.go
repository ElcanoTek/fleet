// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// a2a-tck-shim is the local reverse proxy that makes the official A2A TCK
// (a2aproject/a2a-tck) runnable against a fleet deployment (#1279). It exists
// because two of the TCK's assumptions do not hold for fleet, and neither
// belongs in the engine:
//
//  1. AUTH — the TCK sends no credentials at all (its clients emit only the
//     A2A-Version header), while fleet's A2A surface requires a typed API key.
//     The shim injects X-API-Key on every forwarded request.
//
//  2. SCENARIOS — the TCK drives SUT behavior through messageId prefixes
//     (tck-complete-task-* must genuinely COMPLETE, tck-input-required-* must
//     park in INPUT_REQUIRED), a convention written for a2a-native servers
//     whose executors see the Message object. Fleet's adapter deliberately
//     puts only the message TEXT into the task prompt, so the marker never
//     reaches the model. The shim bridges that: it appends the matching
//     "[[scenario:…]]" fake-llm marker to the message text, and cmd/fake-llm's
//     tck-complete / tck-ask scenarios do the rest. Both message texts and
//     markers stay harness-territory — the engine is untouched.
//
// STRICTLY a test harness: it holds a live API key and rewrites request
// bodies. Never run it anywhere but a loopback conformance rig.
//
// Usage:
//
//	TCK_API_KEY=<fleet_task_…> go run ./scripts/a2a-tck-shim \
//	    [-listen 127.0.0.1:18001] [-target http://127.0.0.1:18000]
//
// Boot fleet with FLEET_PUBLIC_BASE_URL pointing at the shim's listen address
// so the Agent Card's interface URL routes the TCK through it, then run the
// TCK against the shim (see scripts/a2a-tck.sh).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:18001", "address the shim serves on")
	target := flag.String("target", "http://127.0.0.1:18000", "fleet orchestrator base URL")
	flag.Parse()

	key := os.Getenv("TCK_API_KEY")
	if key == "" {
		log.Fatal("TCK_API_KEY is required (a fleet_task_… key; mint one with the admin key via POST /v1/keys)")
	}
	targetURL, err := url.Parse(*target)
	if err != nil {
		log.Fatalf("bad -target: %v", err)
	}

	rp := &httputil.ReverseProxy{
		FlushInterval: -1, // SSE-safe
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Header.Set("X-API-Key", key)
			rewriteScenarioMarkers(pr.Out)
		},
	}

	log.Printf("a2a-tck-shim: %s -> %s (auth injection + messageId scenario markers)", *listen, *target)
	log.Fatal(http.ListenAndServe(*listen, rp)) //nolint:gosec // loopback test harness, not a served endpoint
}

// scenarioForMessageID maps the TCK's messageId prefixes to the fake-llm
// scenario markers cmd/fake-llm registers.
func scenarioForMessageID(id string) string {
	switch {
	case strings.HasPrefix(id, "tck-complete-task"):
		return "tck-complete"
	case strings.HasPrefix(id, "tck-input-required"):
		return "tck-ask"
	case strings.HasPrefix(id, "tck-artifact-text"):
		return "tck-artifact-text"
	// Longest prefix first: tck-artifact-file-url starts with tck-artifact-file.
	// Both map to the same scenario — fleet's published-file artifacts carry
	// URL parts, which both tests accept.
	case strings.HasPrefix(id, "tck-artifact-file"):
		return "tck-artifact-file"
	}
	return ""
}

// rewriteScenarioMarkers appends the fake-llm scenario marker to the message
// text of SendMessage-family JSON-RPC calls whose messageId carries a TCK
// scenario prefix. Anything unparseable passes through untouched — the shim
// must never turn a TCK request into a different failure than the SUT's own.
func rewriteScenarioMarkers(r *http.Request) {
	if r.Method != http.MethodPost || r.Body == nil || !strings.Contains(r.URL.Path, "/a2a") {
		return
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		r.Body = io.NopCloser(bytes.NewReader(nil))
		return
	}
	restore := func(b []byte) {
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.ContentLength = int64(len(b))
		r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	}

	var envelope map[string]json.RawMessage
	if json.Unmarshal(body, &envelope) != nil {
		restore(body)
		return
	}
	var method string
	if json.Unmarshal(envelope["method"], &method) != nil ||
		(method != "SendMessage" && method != "SendStreamingMessage") {
		restore(body)
		return
	}
	var params struct {
		Message map[string]json.RawMessage `json:"message"`
	}
	rawParams, ok := envelope["params"]
	if !ok || json.Unmarshal(rawParams, &params) != nil || params.Message == nil {
		restore(body)
		return
	}
	var messageID string
	_ = json.Unmarshal(params.Message["messageId"], &messageID)
	scenario := scenarioForMessageID(messageID)
	if scenario == "" {
		restore(body)
		return
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(params.Message["parts"], &parts) != nil {
		restore(body)
		return
	}
	marked := false
	for _, part := range parts {
		var text string
		if raw, ok := part["text"]; ok && json.Unmarshal(raw, &text) == nil {
			markedText, _ := json.Marshal(text + " [[scenario:" + scenario + "]]")
			part["text"] = markedText
			marked = true
			break // one marker is enough; fake-llm reads the first
		}
	}
	if !marked {
		restore(body)
		return
	}

	// Re-assemble from the outside in, preserving every field we didn't touch.
	newParts, _ := json.Marshal(parts)
	params.Message["parts"] = newParts
	var paramsMap map[string]json.RawMessage
	if json.Unmarshal(rawParams, &paramsMap) != nil {
		restore(body)
		return
	}
	newMessage, _ := json.Marshal(params.Message)
	paramsMap["message"] = newMessage
	newParams, _ := json.Marshal(paramsMap)
	envelope["params"] = newParams
	newBody, err := json.Marshal(envelope)
	if err != nil {
		restore(body)
		return
	}
	restore(newBody)
}
