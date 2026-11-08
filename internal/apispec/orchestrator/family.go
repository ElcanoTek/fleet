// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

// Package orchestrator is the orchestrator's HTTP contract: the route catalog
// (operations.go) and the hand-maintained base document (base.yaml) that
// internal/apispec generates docs/openapi.yaml from (ADR-0109).
//
// Adding a route to buildOrchestratorMux (cmd/fleet/orchestrator_mux.go)
// means adding its Operation here and running `make api-contracts`;
// TestOpenAPIGenerated (cmd/fleet) fails until both agree.
package orchestrator

import (
	_ "embed"

	"github.com/ElcanoTek/fleet/internal/apispec"
)

// base is docs/openapi.yaml without its paths: info, servers, the
// x-fleet-* extensions and components. Edit it here, never in docs/.
//
//go:embed base.yaml
var base []byte

// V1Prefix is where the public API mounts the orchestrator router on the one
// HTTP surface (cmd/fleet/router.go, ADR-0110). The browser reaches the same
// router at /api/orchestrator; the document describes the /v1 mount, which is
// its servers URL.
const V1Prefix = "/v1"

// Family is the orchestrator's contract for apispec.Generate, walked on the
// one public router.
func Family() apispec.Family {
	return apispec.Family{
		Name:       "orchestrator",
		Prefix:     V1Prefix,
		Base:       base,
		Operations: Operations,
		PathParams: PathParams,
	}
}
