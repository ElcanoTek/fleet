// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package a2a

import (
	"encoding/json"
	"testing"
)

// TestBuildCardShape pins the v1.0 Agent Card contract on the marshaled JSON:
// the generated a2a.json schema carries NO required arrays (protoc-gen-jsonschema
// drops google.api.field_behavior), so requiredness is asserted by hand here —
// and the v0.3 fields that no longer exist must not reappear on an SDK bump.
func TestBuildCardShape(t *testing.T) {
	card := BuildCard(CardSpec{
		Name:        "Larkspur",
		Description: "An AI workspace with real tool use.",
		Version:     "1.2.3",
		RPCURL:      "https://fleet.example.com/v1/a2a",
	})
	body, etag, err := MarshalCard(card)
	if err != nil {
		t.Fatal(err)
	}
	if etag == "" || etag[0] != '"' {
		t.Errorf("etag %q should be a quoted strong validator", etag)
	}
	// Same card, same bytes, same ETag: the card is static per boot and the
	// 304 path depends on that.
	if _, etag2, _ := MarshalCard(card); etag2 != etag {
		t.Errorf("etag not stable: %q vs %q", etag, etag2)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	// Proto-REQUIRED fields (a2a.proto AgentCard, google.api.field_behavior).
	for _, key := range []string{"name", "description", "version", "capabilities",
		"defaultInputModes", "defaultOutputModes", "skills", "supportedInterfaces"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("card is missing required field %q", key)
		}
	}
	// v0.3 fields removed in v1.0 — and the §8.5 sample's "security", which is
	// a confirmed spec doc bug (the field is securityRequirements).
	for _, key := range []string{"protocolVersion", "url", "preferredTransport", "additionalInterfaces", "security"} {
		if _, ok := doc[key]; ok {
			t.Errorf("card carries %q, which does not exist on a v1.0 AgentCard", key)
		}
	}
	if _, ok := doc["securityRequirements"]; !ok {
		t.Error("card is missing securityRequirements")
	}
	// The scopes value must be the proto StringList OBJECT ({"list": [...]}),
	// not the bare array a2a-go v2.5.0 marshals (a TCK card-structure MUST
	// failure until MarshalCard normalized it).
	var reqs []struct {
		Schemes map[string]struct {
			List *[]string `json:"list"`
		} `json:"schemes"`
	}
	if err := json.Unmarshal(doc["securityRequirements"], &reqs); err != nil {
		t.Fatalf("securityRequirements is not the {schemes: {name: {list: []}}} shape: %v", err)
	}
	if len(reqs) != 1 || reqs[0].Schemes["apiKey"].List == nil {
		t.Errorf("securityRequirements scopes must be an object with a non-null list, got %s", doc["securityRequirements"])
	}

	var ifaces []map[string]any
	if err := json.Unmarshal(doc["supportedInterfaces"], &ifaces); err != nil || len(ifaces) != 1 {
		t.Fatalf("supportedInterfaces: %v %v", ifaces, err)
	}
	if ifaces[0]["protocolBinding"] != "JSONRPC" || ifaces[0]["protocolVersion"] != "1.0" ||
		ifaces[0]["url"] != "https://fleet.example.com/v1/a2a" {
		t.Errorf("interface entry wrong: %+v", ifaces[0])
	}

	var caps map[string]any
	if err := json.Unmarshal(doc["capabilities"], &caps); err != nil {
		t.Fatal(err)
	}
	if caps["streaming"] != true {
		t.Errorf("capabilities.streaming = %v, want true", caps["streaming"])
	}
	// Deferred features stay declared-off (absent under omitempty is the same
	// wire fact as false), and the removed v0.3 capability must not appear.
	if v, ok := caps["pushNotifications"]; ok && v != false {
		t.Errorf("capabilities.pushNotifications = %v, want false/absent", v)
	}
	if _, ok := caps["stateTransitionHistory"]; ok {
		t.Error("capabilities.stateTransitionHistory does not exist in v1.0")
	}

	// The declared security scheme is the one the dispatcher actually accepts.
	var schemes map[string]map[string]map[string]any
	if err := json.Unmarshal(doc["securitySchemes"], &schemes); err != nil {
		t.Fatal(err)
	}
	apiKey := schemes["apiKey"]["apiKeySecurityScheme"]
	if apiKey == nil || apiKey["name"] != "X-API-Key" || apiKey["location"] != "header" {
		t.Errorf("securitySchemes.apiKey.apiKeySecurityScheme = %+v, want X-API-Key in header", apiKey)
	}
}
