package main

import (
	"testing"

	"github.com/ElcanoTek/fleet/internal/mcpbroker"
)

// The parent maps the child's public connector metadata into the Manager's
// snapshot; nil (an older child) must stay nil so the reload keeps the boot one.
func TestConnectorMetaFromDescriptors(t *testing.T) {
	if connectorMetaFromDescriptors(nil) != nil {
		t.Fatal("nil descriptors must stay nil so a reload keeps the previous snapshot")
	}
	got := connectorMetaFromDescriptors([]mcpbroker.ConnectorDescriptor{{
		Name: "bq", DisplayName: "BigQuery", Category: "data-storage", Optional: true, Tools: []string{"bq_query"},
	}})
	if len(got) != 1 || got[0].Name != "bq" || got[0].Category != "data-storage" || got[0].Configured || got[0].Tools[0] != "bq_query" {
		t.Fatalf("connectorMetaFromDescriptors = %+v", got)
	}
}
