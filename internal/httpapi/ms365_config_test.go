package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/ms365mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

type fakeMS365ConfigStore struct {
	*store.Store
	clientID string
	tenantID string
	ok       bool
}

func (f *fakeMS365ConfigStore) MS365MCPConfig(_ context.Context) (string, string, bool, error) {
	return f.clientID, f.tenantID, f.ok, nil
}

func (f *fakeMS365ConfigStore) SetMS365MCPConfig(_ context.Context, clientID, tenantID, _ string) error {
	f.clientID = clientID
	f.tenantID = tenantID
	f.ok = true
	return nil
}

func (f *fakeMS365ConfigStore) DeleteMS365MCPConfig(_ context.Context) error {
	f.clientID = ""
	f.tenantID = ""
	f.ok = false
	return nil
}

func TestHandleMS365ConfigAndCatalogProjection(t *testing.T) {
	fakeStore := &fakeMS365ConfigStore{}
	ms365mcp.RegisterCredentialsProvider(func(ctx context.Context) (string, string, bool) {
		cid, tid, ok, _ := fakeStore.MS365MCPConfig(ctx)
		return cid, tid, ok
	})
	defer ms365mcp.RegisterCredentialsProvider(nil)

	srv := &Server{
		cfg: &config.Config{
			PublicBaseURL: "https://fleet.example.com",
			AdminEmails:   []string{"admin@example.com"},
		},
		store: fakeStore,
	}

	// 1. Unconfigured GET returns configured=false, source=none.
	reqGet := httptest.NewRequest(http.MethodGet, "/mcp/ms365-config", nil)
	reqGet = reqGet.WithContext(asUserRole(reqGet.Context(), "admin@example.com", store.RoleAdmin))
	recGet := httptest.NewRecorder()
	srv.handleMS365Config(recGet, reqGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("GET /mcp/ms365-config status = %d", recGet.Code)
	}
	var cfgResp ms365ConfigResponse
	if err := json.Unmarshal(recGet.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("unmarshal GET response: %v", err)
	}
	if cfgResp.Configured || cfgResp.Source != "none" || !cfgResp.CanEdit {
		t.Fatalf("unexpected initial response: %+v", cfgResp)
	}

	// 2. Non-UUID client_id is rejected with 400 Bad Request.
	reqBad := httptest.NewRequest(http.MethodPut, "/mcp/ms365-config", bytes.NewBufferString(`{"client_id":"not-a-uuid","tenant_id":"common"}`))
	reqBad = reqBad.WithContext(asUserRole(reqBad.Context(), "admin@example.com", store.RoleAdmin))
	recBad := httptest.NewRecorder()
	srv.handleMS365Config(recBad, reqBad)
	if recBad.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-UUID client_id, got %d", recBad.Code)
	}

	// 3. Valid UUID client_id + tenant_id saves to store and updates cache.
	validBody := `{"client_id":"68dad9d6-e1aa-4195-9bd5-2e94a0e4a073","tenant_id":"5db5070c-2c68-449d-868f-2b0d3407f6d2"}`
	reqPut := httptest.NewRequest(http.MethodPut, "/mcp/ms365-config", bytes.NewBufferString(validBody))
	reqPut = reqPut.WithContext(asUserRole(reqPut.Context(), "admin@example.com", store.RoleAdmin))
	recPut := httptest.NewRecorder()
	srv.handleMS365Config(recPut, reqPut)
	if recPut.Code != http.StatusOK {
		t.Fatalf("PUT /mcp/ms365-config status = %d: %s", recPut.Code, recPut.Body.String())
	}
	if err := json.Unmarshal(recPut.Body.Bytes(), &cfgResp); err != nil {
		t.Fatalf("unmarshal PUT response: %v", err)
	}
	if !cfgResp.Configured || cfgResp.Source != "db" || cfgResp.ClientID != "68dad9d6-e1aa-4195-9bd5-2e94a0e4a073" {
		t.Fatalf("unexpected saved response: %+v", cfgResp)
	}

	// 4. Environment variable takes precedence over DB when MS365_MCP_CLIENT_ID is set.
	t.Setenv("MS365_MCP_CLIENT_ID", "11111111-2222-3333-4444-555555555555")
	t.Setenv("MS365_MCP_TENANT_ID", "organizations")
	resolvedEnv := srv.resolveMS365Config(context.Background())
	if resolvedEnv.Source != "env" || resolvedEnv.ClientID != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("expected env precedence over db, got %+v", resolvedEnv)
	}
}
