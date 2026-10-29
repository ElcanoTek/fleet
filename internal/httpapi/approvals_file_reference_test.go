//go:build fleet_host_executor

// Regression tests: an approved MCP call staged with a workspace-file
// reference (ADR-0066/0088) must send the referenced bytes, not the
// reference object. runStagedTool used to hand the staged arguments straight
// to the broker, so a critical upload tool (Lens create_job's
// content_base64) received {workspace_file, sha256, offset, length} and
// refused it, although the same call ran fine in a turn with no approval.

package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/store"
)

var lensApprovalCatalog = []mcp.ServerTool{{ServerName: "lens", Tool: mcp.Tool{Name: "create_job", InputSchema: map[string]any{
	"properties": map[string]any{
		"content_base64": map[string]any{"type": "string", "contentEncoding": "base64"},
		"filename":       map[string]any{"type": "string"},
	},
}}}}

// snapshotBroker records a copy of the arguments a call was dispatched with:
// after the call, the approval path puts the references back into the same
// map (agentcore.ResolveWorkspaceFileReferences' done), so the map itself
// no longer shows what was sent.
type snapshotBroker struct {
	text   string
	server string
	tool   string
	args   map[string]any
}

func (b *snapshotBroker) CallMCP(_ context.Context, server, tool string, args map[string]any) (string, bool, error) {
	b.server, b.tool, b.args = server, tool, maps.Clone(args)
	return b.text, false, nil
}

// sandboxApprovalEngine adds a host sandbox pool to approvalEngine, so the
// approval reads the workspace through a real sandbox FileOp.
type sandboxApprovalEngine struct {
	*approvalEngine
	pool *sandbox.Pool
}

func (e *sandboxApprovalEngine) SandboxPool() *sandbox.Pool { return e.pool }

type fileReferenceFixture struct {
	srv    *Server
	broker *snapshotBroker
	path   string
	data   []byte
	conv   string
}

func newFileReferenceFixture(t *testing.T) *fileReferenceFixture {
	t.Helper()
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	st := newFakeChatStore()
	conv, err := st.CreateConversation(context.Background(), "u@x.com", "t", "p", "", false)
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	dir := filepath.Join(os.Getenv("FLEET_WORKSPACE_ROOT"), conv.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("domain\nexample.com\nexample.org\n")
	path := filepath.Join(dir, "pilot.csv")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pool := sandbox.NewPool(sandbox.PoolConfig{Mode: sandbox.ModeHost, EgressProxy: sandbox.NewEgressProxy()})
	t.Cleanup(pool.Close)
	broker := &snapshotBroker{text: `{"ok":true}`}
	srv := New(&config.Config{}, &sandboxApprovalEngine{
		approvalEngine: &approvalEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: lensApprovalCatalog},
		pool:           pool,
	}, st)
	return &fileReferenceFixture{srv: srv, broker: broker, path: path, data: data, conv: conv.ID}
}

// approval stages a create_job whose content_base64 is a reference to the
// window [offset, offset+length) of the fixture file.
func (f *fileReferenceFixture) approval(t *testing.T, offset, length int) *store.Approval {
	t.Helper()
	sum := sha256.Sum256(f.data)
	args, err := json.Marshal(map[string]any{
		"filename":  "pilot.csv",
		"record_id": json.Number("9007199254740993"),
		"content_base64": map[string]any{
			"workspace_file": "pilot.csv", "sha256": hex.EncodeToString(sum[:]), "offset": offset, "length": length,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &store.Approval{UserEmail: "u@x.com", ConversationID: f.conv, ToolName: "mcp_lens_create_job", ArgsJSON: string(args)}
}

func TestRunStagedToolResolvesWorkspaceFileReference(t *testing.T) {
	f := newFileReferenceFixture(t)
	for _, window := range []struct{ offset, length int }{{0, len(f.data)}, {7, 11}} {
		t.Run(fmt.Sprintf("bytes %d+%d", window.offset, window.length), func(t *testing.T) {
			approval := f.approval(t, window.offset, window.length)
			staged := approval.ArgsJSON
			if _, err := f.srv.runStagedTool(context.Background(), approval); err != nil {
				t.Fatalf("runStagedTool: %v", err)
			}
			want := base64.StdEncoding.EncodeToString(f.data[window.offset : window.offset+window.length])
			if got := f.broker.args["content_base64"]; got != want {
				t.Fatalf("content_base64 = %#v, want base64 of the referenced window %q", got, want)
			}
			if got := f.broker.args["record_id"]; got != json.Number("9007199254740993") {
				t.Fatalf("reviewed record ID changed during execution: %v (%T)", got, got)
			}
			if approval.ArgsJSON != staged {
				t.Fatal("stored approval arguments changed; they must keep the reference")
			}
		})
	}
}

func TestRunStagedToolRefusesChangedOrUnreachableFile(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T, *fileReferenceFixture, *store.Approval)
		wantErr string
	}{
		{
			name: "file changed after staging",
			prepare: func(t *testing.T, f *fileReferenceFixture, _ *store.Approval) {
				if err := os.WriteFile(f.path, []byte("domain\nattacker.example\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantErr: "SHA-256 mismatch",
		},
		{
			name: "path outside the workspace",
			prepare: func(t *testing.T, _ *fileReferenceFixture, a *store.Approval) {
				a.ArgsJSON = strings.Replace(a.ArgsJSON, `"workspace_file":"pilot.csv"`, `"workspace_file":"../pilot.csv"`, 1)
				if !strings.Contains(a.ArgsJSON, `"../pilot.csv"`) {
					t.Fatalf("fixture did not rewrite the path: %s", a.ArgsJSON)
				}
			},
			wantErr: "relative path",
		},
		{
			name: "approval with no conversation",
			prepare: func(_ *testing.T, _ *fileReferenceFixture, a *store.Approval) {
				a.ConversationID = ""
			},
			wantErr: "no conversation workspace",
		},
		{
			name: "dot conversation id",
			prepare: func(_ *testing.T, _ *fileReferenceFixture, a *store.Approval) {
				a.ConversationID = "."
			},
			wantErr: "no conversation workspace",
		},
		{
			name: "nested conversation id",
			prepare: func(_ *testing.T, f *fileReferenceFixture, a *store.Approval) {
				a.ConversationID = f.conv + "/nested"
			},
			wantErr: "no conversation workspace",
		},
		{
			name: "fractional offset",
			prepare: func(t *testing.T, _ *fileReferenceFixture, a *store.Approval) {
				a.ArgsJSON = strings.Replace(a.ArgsJSON, `"offset":0`, `"offset":0.5`, 1)
				if !strings.Contains(a.ArgsJSON, `"offset":0.5`) {
					t.Fatalf("fixture did not rewrite the offset: %s", a.ArgsJSON)
				}
			},
			wantErr: "byte range",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFileReferenceFixture(t)
			approval := f.approval(t, 0, len(f.data))
			tc.prepare(t, f, approval)
			_, err := f.srv.runStagedTool(context.Background(), approval)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runStagedTool error = %v, want one mentioning %q", err, tc.wantErr)
			}
			if f.broker.server != "" {
				t.Fatalf("broker was called (%s.%s) for a reference that must not be sent", f.broker.server, f.broker.tool)
			}
		})
	}
}

// retainingBroker keeps the call's arguments past its return, as an
// out-of-process call cancelled mid-flight does until the broker child
// answers (mcp.RetainArguments).
type retainingBroker struct {
	args map[string]any
	end  func()
}

func (b *retainingBroker) CallMCP(ctx context.Context, _, _ string, args map[string]any) (string, bool, error) {
	b.args, b.end = args, mcp.RetainArguments(ctx)
	return `{"ok":true}`, false, nil
}

// The read windows stay held while the dispatch retains the arguments: the
// references are put back (and the windows released) only once the
// retention ends, not when runStagedTool returns.
func TestRunStagedToolHoldsWindowsWhileArgumentsRetained(t *testing.T) {
	f := newFileReferenceFixture(t)
	broker := &retainingBroker{}
	f.srv.agent.(*sandboxApprovalEngine).broker = broker
	if _, err := f.srv.runStagedTool(context.Background(), f.approval(t, 0, len(f.data))); err != nil {
		t.Fatalf("runStagedTool: %v", err)
	}
	if _, ok := broker.args["content_base64"].(string); !ok {
		t.Fatalf("windows released while the dispatch still retains its arguments: %#v", broker.args["content_base64"])
	}
	broker.end()
	if _, ok := broker.args["content_base64"].(map[string]any); !ok {
		t.Fatalf("reference not restored after the retention ended: %#v", broker.args["content_base64"])
	}
}

// A call with no reference never takes a sandbox: the fake engine has no
// pool, so a take would fail the call.
func TestRunStagedToolWithoutReferenceTakesNoSandbox(t *testing.T) {
	broker := &fakeMCPBroker{text: `{"ok":true}`}
	s := &Server{agent: &approvalEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: lensApprovalCatalog}}
	_, err := s.runStagedTool(context.Background(), &store.Approval{
		ToolName: "mcp_lens_create_job",
		ArgsJSON: `{"filename":"pilot.csv","content_base64":"ZG9tYWluCg=="}`,
	})
	if err != nil {
		t.Fatalf("runStagedTool: %v", err)
	}
	if got := broker.args["content_base64"]; got != "ZG9tYWluCg==" {
		t.Fatalf("inline base64 changed: %#v", got)
	}
}
