package sandbox

// k8s_fake_test.go is the fake Kubernetes apiserver the kubernetes-backend
// tests run against: enough of the core/v1 REST surface (pods CRUD, access
// reviews, the preflight objects) plus a v4.channel.k8s.io WebSocket exec
// endpoint whose "processes" are Go handlers. File uploads store bytes; the
// fileops exec pipes them through a REAL host python3 running the uploaded
// script, so the k8s transport is tested against the genuine executor.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const fakeKubeToken = "test-token"

type fakeKube struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	pods    map[string]*k8sPod // name → object (status stamped Running/ready)
	files   map[string][]byte  // "<pod>:<path>" → uploaded bytes
	deleted []string

	// Failure injection for preflight tests.
	denied         map[string]bool // "<verb> <resource>[/<sub>]" → deny
	noPVC          bool
	noNetpol       bool
	absentNetpols  map[string]bool // name → 404, for testing one missing policy
	noRuntimeClass bool

	// bridgeTrailingStdout, when set, is written to the bridge's stdout AFTER
	// each response line — the pod-side output nothing on the fleet side reads.
	bridgeTrailingStdout []byte

	// bashBehaviors maps a bash command string to its fake process. The
	// handler receives the parsed workdir ("" when the call carried none).
	bashBehaviors map[string]func(workdir string, stdout, stderr io.Writer, conn *websocket.Conn) int

	// lastBashWorkdir records the workdir the most recent bash exec carried.
	lastBashWorkdir string
}

func newFakeKube(t *testing.T) *fakeKube {
	t.Helper()
	f := &fakeKube{
		t:             t,
		pods:          make(map[string]*k8sPod),
		files:         make(map[string][]byte),
		denied:        make(map[string]bool),
		absentNetpols: make(map[string]bool),
		bashBehaviors: make(map[string]func(string, io.Writer, io.Writer, *websocket.Conn) int),
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// kubeconfigPath writes a kubeconfig pointing at the fake server (token auth,
// CA pinned to the httptest certificate) and returns its path.
func (f *fakeKube) kubeconfigPath(t *testing.T) string {
	t.Helper()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	dir := t.TempDir()
	path := filepath.Join(dir, "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: fake
contexts:
  - name: fake
    context:
      cluster: fake
      user: fake
clusters:
  - name: fake
    cluster:
      server: %s
      certificate-authority-data: %s
users:
  - name: fake
    user:
      token: %s
`, f.srv.URL, base64.StdEncoding.EncodeToString(caPEM), fakeKubeToken)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// backend builds a KubernetesBackend against the fake server.
func (f *fakeKube) backend(t *testing.T, cfg KubernetesConfig) *KubernetesBackend {
	t.Helper()
	cfg.KubeconfigPath = f.kubeconfigPath(t)
	if cfg.WorkspaceClaim == "" {
		cfg.WorkspaceClaim = "fleet-workspace"
	}
	b, err := NewKubernetesBackend(cfg)
	if err != nil {
		t.Fatalf("NewKubernetesBackend: %v", err)
	}
	return b
}

func (f *fakeKube) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+fakeKubeToken
}

var (
	podPathRe    = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods/([^/]+)$`)
	podExecRe    = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods/([^/]+)/exec$`)
	podListRe    = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/pods$`)
	pvcPathRe    = regexp.MustCompile(`^/api/v1/namespaces/([^/]+)/persistentvolumeclaims/([^/]+)$`)
	netpolRe     = regexp.MustCompile(`^/apis/networking\.k8s\.io/v1/namespaces/([^/]+)/networkpolicies/([^/]+)$`)
	runtimeClsRe = regexp.MustCompile(`^/apis/node\.k8s\.io/v1/runtimeclasses/([^/]+)$`)
)

func writeK8sStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "apiVersion": "v1", "reason": reason, "message": msg, "code": code,
	})
}

func (f *fakeKube) handle(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		writeK8sStatus(w, http.StatusUnauthorized, "Unauthorized", "bad token")
		return
	}
	path := r.URL.Path
	switch {
	case path == "/version":
		_, _ = w.Write([]byte(`{"gitVersion":"v1.31.0-fake"}`))
	case path == "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews":
		f.handleAccessReview(w, r)
	case podExecRe.MatchString(path):
		f.handleExec(w, r)
	case podPathRe.MatchString(path):
		f.handlePod(w, r)
	case podListRe.MatchString(path):
		if r.Method == http.MethodPost {
			f.handleCreatePod(w, r)
			return
		}
		f.handleListPods(w, r)
	case pvcPathRe.MatchString(path):
		if f.noPVC {
			writeK8sStatus(w, http.StatusNotFound, "NotFound", "pvc not found")
			return
		}
		_, _ = w.Write([]byte(`{"kind":"PersistentVolumeClaim"}`))
	case netpolRe.MatchString(path):
		if f.noNetpol || f.absentNetpols[netpolRe.FindStringSubmatch(path)[2]] {
			writeK8sStatus(w, http.StatusNotFound, "NotFound", "networkpolicy not found")
			return
		}
		_, _ = w.Write([]byte(`{"kind":"NetworkPolicy"}`))
	case runtimeClsRe.MatchString(path):
		if f.noRuntimeClass {
			writeK8sStatus(w, http.StatusNotFound, "NotFound", "runtimeclass not found")
			return
		}
		_, _ = w.Write([]byte(`{"kind":"RuntimeClass"}`))
	default:
		writeK8sStatus(w, http.StatusNotFound, "NotFound", "no fake route for "+path)
	}
}

func (f *fakeKube) handleAccessReview(w http.ResponseWriter, r *http.Request) {
	var review struct {
		Spec struct {
			ResourceAttributes struct {
				Verb        string `json:"verb"`
				Resource    string `json:"resource"`
				Subresource string `json:"subresource"`
			} `json:"resourceAttributes"`
		} `json:"spec"`
	}
	_ = json.NewDecoder(r.Body).Decode(&review)
	key := review.Spec.ResourceAttributes.Verb + " " + review.Spec.ResourceAttributes.Resource
	if review.Spec.ResourceAttributes.Subresource != "" {
		key += "/" + review.Spec.ResourceAttributes.Subresource
	}
	f.mu.Lock()
	allowed := !f.denied[key]
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"allowed": allowed}})
}

func (f *fakeKube) handleCreatePod(w http.ResponseWriter, r *http.Request) {
	var pod k8sPod
	if err := json.NewDecoder(r.Body).Decode(&pod); err != nil {
		writeK8sStatus(w, http.StatusBadRequest, "BadRequest", err.Error())
		return
	}
	pod.Status = k8sPodStatus{
		Phase: "Running",
		ContainerStatuses: []k8sContainerStatus{
			{Name: sandboxContainerName, Ready: true},
		},
	}
	f.mu.Lock()
	f.pods[pod.Metadata.Name] = &pod
	f.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(&pod)
}

func (f *fakeKube) handlePod(w http.ResponseWriter, r *http.Request) {
	m := podPathRe.FindStringSubmatch(r.URL.Path)
	name := m[2]
	f.mu.Lock()
	pod, ok := f.pods[name]
	f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		if !ok {
			writeK8sStatus(w, http.StatusNotFound, "NotFound", "pod not found")
			return
		}
		_ = json.NewEncoder(w).Encode(pod)
	case http.MethodDelete:
		if !ok {
			writeK8sStatus(w, http.StatusNotFound, "NotFound", "pod not found")
			return
		}
		f.mu.Lock()
		delete(f.pods, name)
		f.deleted = append(f.deleted, name)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"kind": "Status", "status": "Success"})
	default:
		writeK8sStatus(w, http.StatusMethodNotAllowed, "MethodNotAllowed", r.Method)
	}
}

func (f *fakeKube) handleListPods(w http.ResponseWriter, r *http.Request) {
	selector := r.URL.Query().Get("labelSelector")
	var want [][2]string
	if selector != "" {
		for _, kv := range strings.Split(selector, ",") {
			k, v, _ := strings.Cut(kv, "=")
			want = append(want, [2]string{k, v})
		}
	}
	list := k8sPodList{Items: []k8sPod{}}
	f.mu.Lock()
	for _, pod := range f.pods {
		match := true
		for _, kv := range want {
			if pod.Metadata.Labels[kv[0]] != kv[1] {
				match = false
				break
			}
		}
		if match {
			list.Items = append(list.Items, *pod)
		}
	}
	f.mu.Unlock()
	_ = json.NewEncoder(w).Encode(&list)
}

// ── exec ──

var (
	uploadRe  = regexp.MustCompile(`^head -c (\d+) > (\S+) && wc -c < (\S+)$`)
	fileOpsRe = regexp.MustCompile(`^head -c (\d+) \| python3 (\S+)$`)
)

// The channel-protocol constants moved out of production code when exec went
// through client-go (the executor owns the framing); the fake still speaks the
// raw protocol because it IS the emulated server side.
const (
	// client-go's executor speaks v5 only (v4 + a stream-close signal); a
	// modern kubelet offers it, so the fake does too.
	k8sExecProtocolV5 = "v5.channel.k8s.io"

	k8sChannelStdin  = 0
	k8sChannelStdout = 1
	k8sChannelStderr = 2
	k8sChannelError  = 3

	// k8sStreamClose is v5's half-close: a two-byte message [255, streamID].
	k8sStreamClose = 255
)

var execUpgrader = websocket.Upgrader{Subprotocols: []string{k8sExecProtocolV5}}

// execConn wraps the server side of one exec connection.
type execConn struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (e *execConn) send(channel byte, data []byte) {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	_ = e.conn.WriteMessage(websocket.BinaryMessage, append([]byte{channel}, data...))
}

func (e *execConn) finish(exitCode int) {
	var status []byte
	if exitCode == 0 {
		status = []byte(`{"metadata":{},"status":"Success"}`)
	} else {
		status = []byte(fmt.Sprintf(`{"metadata":{},"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"%d"}]}}`, exitCode))
	}
	e.send(k8sChannelError, status)
	e.writeMu.Lock()
	_ = e.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), timeNowPlusSecond())
	e.writeMu.Unlock()
	_ = e.conn.Close()
}

// readStdin reads stdin frames until n bytes have arrived, the client
// half-closes stdin (the v5 [255, 0] signal — how a real kubelet learns the
// payload is complete), or the conn ends.
func (e *execConn) readStdin(n int) []byte {
	var buf bytes.Buffer
	for buf.Len() < n {
		_, data, err := e.conn.ReadMessage()
		if err != nil {
			break
		}
		if len(data) == 2 && data[0] == k8sStreamClose && data[1] == k8sChannelStdin {
			break
		}
		if len(data) > 0 && data[0] == k8sChannelStdin {
			buf.Write(data[1:])
		}
	}
	return buf.Bytes()
}

func (f *fakeKube) handleExec(w http.ResponseWriter, r *http.Request) {
	m := podExecRe.FindStringSubmatch(r.URL.Path)
	podName := m[2]
	f.mu.Lock()
	_, podExists := f.pods[podName]
	f.mu.Unlock()
	if !podExists {
		writeK8sStatus(w, http.StatusNotFound, "NotFound", "pod not found")
		return
	}
	command := r.URL.Query()["command"]
	conn, err := execUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	e := &execConn{conn: conn}
	f.runFakeProcess(podName, command, e)
}

//nolint:gocognit // test fixture dispatch over the fake process kinds
func (f *fakeKube) runFakeProcess(podName string, command []string, e *execConn) {
	// Bridge session: read JSON lines, respond with canned bridge responses.
	if len(command) == 2 && command[0] == "python3" && command[1] == k8sBridgePath {
		f.runFakeBridge(podName, e)
		return
	}

	// Shell one-shots.
	if len(command) >= 3 && (command[0] == "/bin/sh" || command[0] == "sh") && command[1] == "-c" {
		script := command[2]
		if um := uploadRe.FindStringSubmatch(script); um != nil {
			n, _ := strconv.Atoi(um[1])
			data := e.readStdin(n)
			f.mu.Lock()
			f.files[podName+":"+um[2]] = data
			f.mu.Unlock()
			e.send(k8sChannelStdout, []byte(strconv.Itoa(len(data))+"\n"))
			e.finish(0)
			return
		}
		if fm := fileOpsRe.FindStringSubmatch(script); fm != nil {
			n, _ := strconv.Atoi(fm[1])
			req := e.readStdin(n)
			f.mu.Lock()
			script := f.files[podName+":"+fm[2]]
			f.mu.Unlock()
			if script == nil {
				e.send(k8sChannelStderr, []byte("fileops script not uploaded"))
				e.finish(1)
				return
			}
			// Run the REAL uploaded fileops.py on the test host so the k8s
			// transport is exercised against the genuine executor.
			cmd := exec.Command("python3", "-c", string(script))
			cmd.Stdin = bytes.NewReader(req)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
				} else {
					e.send(k8sChannelStderr, []byte(err.Error()))
					e.finish(127)
					return
				}
			}
			e.send(k8sChannelStdout, stdout.Bytes())
			if stderr.Len() > 0 {
				e.send(k8sChannelStderr, stderr.Bytes())
			}
			e.finish(code)
			return
		}
		// Workdir-wrapped bash: ["/bin/sh","-c",script,"fleet-bash",dir,cmd]
		if len(command) == 6 && command[3] == "fleet-bash" {
			f.dispatchBash(command[5], command[4], e)
			return
		}
	}
	if len(command) == 3 && command[0] == "bash" && command[1] == "-c" {
		f.dispatchBash(command[2], "", e)
		return
	}
	e.send(k8sChannelStderr, []byte(fmt.Sprintf("fake apiserver: unhandled command %q", command)))
	e.finish(127)
}

func (f *fakeKube) dispatchBash(cmd, workdir string, e *execConn) {
	f.mu.Lock()
	f.lastBashWorkdir = workdir
	behavior := f.bashBehaviors[cmd]
	f.mu.Unlock()
	if behavior == nil {
		e.send(k8sChannelStderr, []byte("fake apiserver: no behavior for bash command "+cmd))
		e.finish(127)
		return
	}
	stdout := &channelWriter{e: e, channel: k8sChannelStdout}
	stderr := &channelWriter{e: e, channel: k8sChannelStderr}
	e.finish(behavior(workdir, stdout, stderr, e.conn))
}

// runFakeBridge speaks the bridge line protocol: each request line gets a
// canned success response echoing the code back in `result`.
func (f *fakeKube) runFakeBridge(_ string, e *execConn) {
	var pending bytes.Buffer
	for {
		_, data, err := e.conn.ReadMessage()
		if err != nil {
			_ = e.conn.Close()
			return
		}
		if len(data) == 0 || data[0] != k8sChannelStdin {
			continue
		}
		pending.Write(data[1:])
		for {
			line, rest, found := bytes.Cut(pending.Bytes(), []byte("\n"))
			if !found {
				break
			}
			var req bridgeRequest
			if err := json.Unmarshal(line, &req); err != nil {
				e.send(k8sChannelStderr, []byte("bad bridge request: "+err.Error()))
				pending = *bytes.NewBuffer(append([]byte(nil), rest...))
				continue
			}
			resp, _ := json.Marshal(bridgeResponse{Status: "ok", Result: "ran: " + req.Code})
			// Optionally append stray stdout AFTER the response newline, in the
			// SAME frame. A real pod does this whenever anything writes to the
			// bridge's fd 1, and one frame is the deterministic case: the demux
			// loop issues a single pipe Write for it, the response reader stops
			// at the first newline, and the remainder of that one Write has
			// nobody left to consume it.
			frame := make([]byte, 0, len(resp)+1+len(f.bridgeTrailingStdout))
			frame = append(frame, resp...)
			frame = append(frame, '\n')
			frame = append(frame, f.bridgeTrailingStdout...)
			e.send(k8sChannelStdout, frame)
			pending = *bytes.NewBuffer(append([]byte(nil), rest...))
		}
	}
}

type channelWriter struct {
	e       *execConn
	channel byte
}

func (c *channelWriter) Write(p []byte) (int, error) {
	c.e.send(c.channel, p)
	return len(p), nil
}

// pythonAvailable reports whether the test host has python3 (the fileops
// integration tests need it; CI always does — it runs the host-executor
// suite).
func pythonAvailable() bool {
	_, err := exec.LookPath("python3")
	return err == nil
}

func timeNowPlusSecond() time.Time { return time.Now().Add(time.Second) }
