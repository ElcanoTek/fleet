package httpapi

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	webpushgo "github.com/SherClockHolmes/webpush-go"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/webpush"
)

// These tests cover approval progress (docs/APPROVAL-PROGRESS.md): an
// approved call of a tool in critical_tool_progress asks for MCP progress,
// the updates cross the broker and land on the executing row, throttled, and
// the card payloads carry them only while the call runs.

func useProgressPolicy(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		CriticalToolSuffixes: []string{"create_deal"},
		CriticalToolProgress: []string{"create_deal"},
	})
}

func useProgressWriteEvery(t *testing.T, d time.Duration) {
	t.Helper()
	prev := approvalProgressWriteEvery
	approvalProgressWriteEvery = d
	t.Cleanup(func() { approvalProgressWriteEvery = prev })
}

// progressStore records SetApprovalProgress writes.
type progressStore struct {
	chatStore
	mu     sync.Mutex
	writes []approvalProgress
	wrote  chan struct{} // receives once per write, if set
}

func (p *progressStore) SetApprovalProgress(_ context.Context, user, id, raw string) (bool, error) {
	var v approvalProgress
	_ = json.Unmarshal([]byte(raw), &v)
	p.mu.Lock()
	p.writes = append(p.writes, v)
	p.mu.Unlock()
	if p.wrote != nil {
		select {
		case p.wrote <- struct{}{}:
		default:
		}
	}
	return user == "u@example.com" && id == "ap1", nil
}

func (p *progressStore) snapshot() []approvalProgress {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]approvalProgress(nil), p.writes...)
}

// progressChild is the credential-owning side: it reports progress through
// the sink its call context carries (as the stdio transport would), waits
// until the parent has stored it, and answers.
type progressChild struct {
	mcpbroker.Backend
	stored  chan struct{}
	sawSink bool
}

func (c *progressChild) CallMCP(ctx context.Context, _, _ string, _ map[string]any) (string, bool, error) {
	sink := mcp.ProgressSink(ctx)
	if sink == nil {
		return "no progress asked", false, nil
	}
	c.sawSink = true
	sink(mcp.ProgressUpdate{Progress: 12, Total: 24, Message: "creating deal 12"})
	select {
	case <-c.stored:
	case <-time.After(5 * time.Second):
		return "", false, context.DeadlineExceeded
	}
	return "created 24 deals", false, nil
}

// An approved call of an opted-in tool asks for progress over the real broker
// protocol, and the update the server sent is stored on the executing row
// before the call returns.
func TestRunStagedTool_ProgressCrossesTheBrokerAndIsStored(t *testing.T) {
	useProgressPolicy(t)
	st := &progressStore{wrote: make(chan struct{}, 1)}
	child := &progressChild{stored: st.wrote}
	clientConn, serverConn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mcpbroker.NewServer(child).Serve(ctx, serverConn)
	}()
	client := mcpbroker.NewClient(clientConn)
	t.Cleanup(func() { _ = client.Close(); cancel(); <-done })
	s := &Server{store: st, agent: &approvalEngine{fakeEngine: &fakeEngine{}, broker: client, catalog: dealsApprovalCatalog}}

	text, err := s.runStagedTool(context.Background(), &store.Approval{
		ID: "ap1", UserEmail: "u@example.com", ToolName: "mcp_deals_mcp_tunnl_create_deal", ArgsJSON: `{}`,
	})
	if err != nil || text != "created 24 deals" {
		t.Fatalf("runStagedTool = %q, %v", text, err)
	}
	if !child.sawSink {
		t.Fatal("the opted-in call must ask the child for progress")
	}
	got := st.snapshot()
	if len(got) != 1 || got[0].Progress != 12 || got[0].Total != 24 || got[0].Message != "creating deal 12" || got[0].UpdatedAt == 0 {
		t.Fatalf("stored progress = %+v", got)
	}
}

// A tool that did not opt in asks for nothing: the call crosses exactly as
// before and nothing is written.
func TestRunStagedTool_NoProgressForOtherTools(t *testing.T) {
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"create_deal"}})
	t.Cleanup(func() { agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{}) })
	st := &progressStore{}
	child := &progressChild{}
	s := &Server{store: st, agent: &approvalEngine{fakeEngine: &fakeEngine{}, broker: child, catalog: dealsApprovalCatalog}}
	text, err := s.runStagedTool(context.Background(), &store.Approval{
		ID: "ap1", UserEmail: "u@example.com", ToolName: "mcp_deals_mcp_tunnl_create_deal", ArgsJSON: `{}`,
	})
	if err != nil || text != "no progress asked" || child.sawSink || len(st.snapshot()) != 0 {
		t.Fatalf("runStagedTool = %q, %v, sink=%v writes=%v", text, err, child.sawSink, st.snapshot())
	}
}

// The writer stores the first update at once and then at most one per
// interval, always the latest; after stop nothing more is written.
func TestStartApprovalProgress_ThrottlesToTheLatest(t *testing.T) {
	useProgressWriteEvery(t, 50*time.Millisecond)
	st := &progressStore{wrote: make(chan struct{}, 4)}
	s := &Server{store: st}
	ctx, stop := s.startApprovalProgress(context.Background(), &store.Approval{ID: "ap1", UserEmail: "u@example.com"})
	sink := mcp.ProgressSink(ctx)
	if sink == nil {
		t.Fatal("startApprovalProgress must attach a sink")
	}
	sink(mcp.ProgressUpdate{Progress: 1, Total: 24})
	waitWrites(t, st, 1)
	sink(mcp.ProgressUpdate{Progress: 2, Total: 24})
	sink(mcp.ProgressUpdate{Progress: 3, Total: 24})
	waitWrites(t, st, 2)
	stop()
	sink(mcp.ProgressUpdate{Progress: 4, Total: 24})
	time.Sleep(120 * time.Millisecond)
	got := st.snapshot()
	if len(got) != 2 || got[0].Progress != 1 || got[1].Progress != 3 {
		t.Fatalf("writes = %+v, want 1 then the latest held (3), and nothing after stop", got)
	}
	stop() // idempotent
}

func waitWrites(t *testing.T, st *progressStore, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for len(st.snapshot()) < n {
		select {
		case <-st.wrote:
		case <-deadline:
			t.Fatalf("writes = %+v, want %d", st.snapshot(), n)
		}
	}
}

// A progress message is plain, bounded text: control and bidi characters
// become spaces, a long one is cut, and one carrying a secret is dropped.
func TestProgressMessage(t *testing.T) {
	if got := progressMessage("deal\u202e 12\n\tof 24"); got != "deal 12 of 24" {
		t.Errorf("sanitized = %q", got)
	}
	if got := progressMessage(strings.Repeat("x", 500)); len([]rune(got)) != approvalProgressMessageMax+1 {
		t.Errorf("long message runes = %d", len([]rune(got)))
	}
	if got := progressMessage("token " + "ghp_" + strings.Repeat("a1B2", 9)); got != "" {
		t.Errorf("a secret-looking message must be dropped, got %q", got)
	}
	if _, ok := canonicalApprovalProgress(approvalProgress{Progress: -1}); ok {
		t.Error("a negative progress must not be shown")
	}
}

// Only an executing row of an opted-in tool carries progress_updates, plus
// its latest progress when there is one; the pending card of such a tool
// carries the flag too, and nothing else changes.
func TestWithApprovalProgress(t *testing.T) {
	useProgressPolicy(t)
	s := &Server{}
	stored := `{"progress":12,"total":24,"message":"creating deal 12","updated_at":5}`
	a := &store.Approval{ToolName: "mcp_deals_create_deal", ProgressJSON: stored}

	got := s.withApprovalProgress(map[string]any{"executing": true}, a)
	p, _ := got["progress"].(*approvalProgress)
	if got["progress_updates"] != true || p == nil || p.Progress != 12 || p.Total != 24 {
		t.Fatalf("executing payload = %+v", got)
	}
	if got := s.withApprovalProgress(map[string]any{"status": "approved", "is_err": false}, a); len(got) != 2 {
		t.Fatalf("a settled row must gain nothing: %+v", got)
	}
	other := &store.Approval{ToolName: "mcp_pages_deploy_page", ProgressJSON: stored}
	if got := s.withApprovalProgress(map[string]any{"executing": true}, other); len(got) != 1 {
		t.Fatalf("a tool that did not opt in must gain nothing: %+v", got)
	}
	bad := &store.Approval{ToolName: "mcp_deals_create_deal", ProgressJSON: `{"progress":"x"}`}
	if got := s.withApprovalProgress(map[string]any{"executing": true}, bad); got["progress"] != nil || got["progress_updates"] != true {
		t.Fatalf("malformed stored progress must not be served: %+v", got)
	}
	if approvalClientFields("mcp_deals_create_deal", `{}`, "c1")["progress_updates"] != true {
		t.Fatal("the pending card of an opted-in tool must carry progress_updates")
	}
	if _, ok := approvalClientFields("mcp_pages_deploy_page", `{}`, "c1")["progress_updates"]; ok {
		t.Fatal("other cards must carry no progress_updates key")
	}
}

// A push names the call by its readable card title when there is one, else
// by the tool, and never by its arguments.
func TestApprovalPushLabel(t *testing.T) {
	withCard := &store.Approval{ToolName: "mcp_deals_create_deal", ArgsJSON: `{"secret_arg":"x"}`, CardJSON: `{"title":"Create 24 deals on PubMatic","items":[]}`}
	if got := approvalPushLabel(withCard); got != "Create 24 deals on PubMatic" {
		t.Fatalf("label = %q", got)
	}
	if got := approvalPushLabel(&store.Approval{ToolName: "mcp_deals_create_deal", ArgsJSON: `{"secret_arg":"x"}`}); got != "mcp_deals_create_deal" {
		t.Fatalf("label = %q", got)
	}
}

// pushRelay is a Web Push relay the test owns: a real webpush.Service sends
// to it (VAPID signing and payload encryption run for real), and it records
// each notification's urgency.
type pushRelay struct {
	mu        sync.Mutex
	urgencies []string
}

func (p *pushRelay) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.urgencies...)
}

type relaySubscriptions struct{ sub store.PushSubscription }

func (r relaySubscriptions) ListPushSubscriptions(_ context.Context, user string) ([]store.PushSubscription, error) {
	if user != "u@example.com" {
		return nil, nil
	}
	return []store.PushSubscription{r.sub}, nil
}
func (relaySubscriptions) DeletePushSubscription(context.Context, string) error { return nil }

func newPushRelay(t *testing.T) (*pushRelay, *webpush.Service) {
	t.Helper()
	relay := &pushRelay{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		relay.mu.Lock()
		relay.urgencies = append(relay.urgencies, r.Header.Get("Urgency"))
		relay.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)
	priv, pub, err := webpushgo.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	sub := store.PushSubscription{
		UserEmail: "u@example.com", Endpoint: srv.URL + "/ep",
		KeysAuth:   base64.RawURLEncoding.EncodeToString(auth),
		KeysP256dh: base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
	}
	svc := webpush.New(webpush.Config{
		VAPIDPublicKey: pub, VAPIDPrivateKey: priv, Contact: "mailto:ops@example.com",
		OnApprovalRequest: true,
	}, relaySubscriptions{sub: sub})
	return relay, svc
}

// The owner gets a normal-urgency push once an opted-in approved call has
// recorded its outcome; a call of any other tool sends none.
func TestExecuteClaimedApproval_PushesWhenAnOptedInCallFinishes(t *testing.T) {
	useProgressPolicy(t)
	relay, svc := newPushRelay(t)
	st := newGroupStore(
		store.Approval{ID: "ap-deal", ConversationID: "c1", ToolName: "mcp_deals_create_deal", Status: "approved", ResultText: approvalExecutingSentinel},
		store.Approval{ID: "ap-page", ConversationID: "c1", ToolName: "mcp_pages_deploy_page", Status: "approved", ResultText: approvalExecutingSentinel},
	)
	s := &Server{store: st, cfg: &config.Config{MockMode: true}, push: svc}
	for _, id := range []string{"ap-deal", "ap-page"} {
		a := st.state(id)
		if out := s.executeClaimedApproval(context.Background(), "u@example.com", "c1", id, &a, approvalRequest{Approved: true}); out["is_err"] != false {
			t.Fatalf("%s outcome = %+v", id, out)
		}
	}
	s.background.StopAndWait()
	if got := relay.seen(); len(got) != 1 || got[0] != "normal" {
		t.Fatalf("pushes = %v, want exactly one normal-urgency push for the opted-in call", got)
	}
}

// resumeSkipStore answers the resume claim with a skip over the hourly cap.
type resumeSkipStore struct {
	chatStore
	skipped string
}

func (r resumeSkipStore) ClaimApprovalResume(context.Context, store.ApprovalResumeRequest) (store.ApprovalResumeResult, error) {
	return store.ApprovalResumeResult{
		Claimed: []store.ApprovalResumeItem{{ID: "ap1", ToolName: "mcp_deals_create_deal", Outcome: "approved"}},
		Skipped: r.skipped, Owner: "u@example.com",
	}, nil
}

// A resume blocked by the hourly cap (or a full queue) tells the owner with a
// high-urgency push: the task now waits for them. A deleted conversation does not.
func TestDecideApprovalResume_PushesWhenTheContinueIsSkipped(t *testing.T) {
	for skipped, want := range map[string]int{
		store.ApprovalResumeSkipRateLimited:      1,
		store.ApprovalResumeSkipQueueFull:        1,
		store.ApprovalResumeSkipConversationGone: 0,
	} {
		t.Run(skipped, func(t *testing.T) {
			relay, svc := newPushRelay(t)
			s := &Server{store: resumeSkipStore{skipped: skipped}, push: svc}
			s.decideApprovalResume("c1", 0)
			s.background.StopAndWait()
			got := relay.seen()
			if len(got) != want || (want == 1 && got[0] != "high") {
				t.Fatalf("pushes = %v, want %d high-urgency", got, want)
			}
		})
	}
}

// A call of a tool that reports progress answers "executing" almost at once,
// even on a server that declared no approved-call budget, so the card can
// start showing its progress (Codex P2 on #1720). The long 50 s threshold is
// left as is: only the progress rule can answer this early.
func TestHandleApproval_ProgressCallAnswersExecutingEarly(t *testing.T) {
	useProgressPolicy(t)
	prev := approvalProgressEarlyReplyAfter
	approvalProgressEarlyReplyAfter = 10 * time.Millisecond
	t.Cleanup(func() { approvalProgressEarlyReplyAfter = prev })
	broker := &budgetBroker{text: "created", entered: make(chan struct{}), release: make(chan struct{})}
	approval := *dealsApproval(`{"name":"one"}`)
	approval.ToolName = "mcp_deals_mcp_tunnl_create_deal"
	st := &claimStore{approval: approval}
	s := &Server{store: st, agent: &budgetEngine{fakeEngine: &fakeEngine{}, broker: broker, catalog: dealsApprovalCatalog}}
	if wait, ok := s.approvalEarlyReplyWait(&approval); !ok || wait != 10*time.Millisecond {
		t.Fatalf("early reply = %v, %v; want the progress threshold", wait, ok)
	}

	first := postExpectingEarlyReply(t, s, broker)
	if first["status"] != "approved" || first["executing"] != true {
		t.Fatalf("first POST = %v, want executing while the call runs", first)
	}
	close(broker.release)
	drainApprovals(t, s)
	if final := postApproval(t, s); final["is_err"] != false {
		t.Fatalf("after the call = %v, want the recorded success", final)
	}

	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{CriticalToolSuffixes: []string{"create_deal"}})
	if _, ok := s.approvalEarlyReplyWait(&approval); ok {
		t.Fatal("without progress or a declared budget the POST must wait for the outcome, as before")
	}
}
