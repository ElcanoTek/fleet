// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// k8s_client.go is the minimal Kubernetes API client the kubernetes sandbox
// backend (#989) uses to create, inspect, delete, and exec into sandbox Pods.
//
// It is deliberately NOT client-go. The backend needs exactly five verbs —
// create/get/delete/list on pods, plus the exec subresource — and client-go
// would add several dozen modules to a dependency tree that is gated by
// govulncheck and a container CVE scan. Everything here is plain net/http
// against the well-versioned core/v1 REST surface, plus gorilla/websocket
// (already in the tree) for exec streaming. The trade is accepted and
// recorded in the ADR: if the backend ever needs watches, informers, or
// exotic auth, revisit client-go rather than growing this file into one.
//
// Auth is loaded from the standard in-cluster mount
// (/var/run/secrets/kubernetes.io/serviceaccount) when no kubeconfig is
// configured, else from a kubeconfig file supporting token and client-cert
// credentials. exec-plugin and auth-provider kubeconfigs are refused with an
// actionable error — the control plane runs unattended, so credentials that
// shell out to an interactive helper cannot work anyway. Fail closed, never
// guess.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// inClusterTokenFile / inClusterCAFile / inClusterNamespaceFile are the
// standard projected service-account mount every Pod gets. The token file is
// re-read per request (see bearerToken) because bound tokens rotate — a
// long-lived fleet process holding the boot-time token would start getting
// 401s about an hour in.
const (
	inClusterTokenFile     = "/var/run/secrets/kubernetes.io/serviceaccount/token" //nolint:gosec // well-known mount path, not a credential
	inClusterCAFile        = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// k8sRequestTimeout bounds any single non-streaming API request. Generous for
// a loaded apiserver; short enough that a dead one fails a turn promptly
// instead of hanging it.
const k8sRequestTimeout = 30 * time.Second

// k8sClient is the minimal REST client. Safe for concurrent use.
type k8sClient struct {
	baseURL *url.URL
	httpc   *http.Client
	// tlsConfig is retained for the websocket dialer (exec), which cannot
	// share http.Transport.
	tlsConfig *tls.Config

	// Exactly one of the following is set. staticToken is a kubeconfig token;
	// tokenFile is re-read per request so rotated bound tokens keep working.
	// Client-cert auth lives inside tlsConfig and needs neither.
	staticToken string
	tokenFile   string

	tokenMu     sync.Mutex
	cachedToken string
	tokenRead   time.Time
}

// tokenRefreshInterval is how long a token-file read is trusted before the
// file is consulted again. Kubernetes rotates bound tokens well before their
// ~1h expiry, so a 1-minute cache never serves a stale token while keeping
// the common path free of file I/O.
const tokenRefreshInterval = time.Minute

// bearerToken returns the Authorization bearer value for a request, or ""
// when client-cert auth is in use.
func (c *k8sClient) bearerToken() (string, error) {
	if c.staticToken != "" {
		return c.staticToken, nil
	}
	if c.tokenFile == "" {
		return "", nil
	}
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	if c.cachedToken != "" && time.Since(c.tokenRead) < tokenRefreshInterval {
		return c.cachedToken, nil
	}
	raw, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("read service-account token: %w", err)
	}
	c.cachedToken = strings.TrimSpace(string(raw))
	c.tokenRead = time.Now()
	return c.cachedToken, nil
}

// newInClusterClient builds a client from the standard service-account mount.
func newInClusterClient() (*k8sClient, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, fmt.Errorf("not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset) and no kubeconfig configured")
	}
	caPEM, err := os.ReadFile(inClusterCAFile)
	if err != nil {
		return nil, fmt.Errorf("read in-cluster CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("in-cluster CA at %s contains no usable certificates", inClusterCAFile)
	}
	if _, err := os.Stat(inClusterTokenFile); err != nil {
		return nil, fmt.Errorf("in-cluster service-account token: %w", err)
	}
	base, err := url.Parse("https://" + net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("parse in-cluster apiserver address: %w", err)
	}
	tlsCfg := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	return &k8sClient{
		baseURL:   base,
		tlsConfig: tlsCfg,
		tokenFile: inClusterTokenFile,
		httpc: &http.Client{
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
		},
	}, nil
}

// inClusterNamespace reads the namespace the control plane itself runs in,
// used only as a default when the sandbox namespace is not configured.
func inClusterNamespace() string {
	raw, err := os.ReadFile(inClusterNamespaceFile)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// k8sStatusError is a non-2xx API response, carrying enough of the
// metav1.Status body to be actionable in logs and boot errors.
type k8sStatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *k8sStatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kubernetes API error %d (%s): %s", e.Code, e.Reason, e.Message)
	}
	return fmt.Sprintf("kubernetes API error %d (%s)", e.Code, e.Reason)
}

// isK8sNotFound reports whether err is a 404 from the API — the pod-already-
// gone case teardown paths treat as success, mirroring containerAlreadyGone.
func isK8sNotFound(err error) bool {
	var se *k8sStatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// do performs one JSON API request. body may be nil. A non-2xx response is
// returned as *k8sStatusError with the server's status message decoded.
func (c *k8sClient) do(ctx context.Context, method, apiPath string, query url.Values, body []byte) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, k8sRequestTimeout)
	defer cancel()

	u := *c.baseURL
	u.Path = path.Join(u.Path, apiPath)
	if query != nil {
		u.RawQuery = query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, u.String(), rdr)
	if err != nil {
		return nil, fmt.Errorf("build %s %s: %w", method, apiPath, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	token, err := c.bearerToken()
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, apiPath, err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Responses are bounded reads: pod objects are a few KB; even a large list
	// stays far under this. Guards against a misbehaving endpoint, not real use.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read response: %w", method, apiPath, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &status)
		return nil, &k8sStatusError{Code: resp.StatusCode, Reason: status.Reason, Message: status.Message}
	}
	return data, nil
}

// ── typed pod surface (the narrow slice of core/v1 the backend touches) ──

type k8sPod struct {
	Metadata k8sObjectMeta `json:"metadata"`
	Spec     k8sPodSpec    `json:"spec"`
	Status   k8sPodStatus  `json:"status,omitempty"`

	// APIVersion/Kind are emitted on create; ignored on read.
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

type k8sObjectMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type k8sPodSpec struct {
	RestartPolicy                 string             `json:"restartPolicy,omitempty"`
	AutomountServiceAccountToken  *bool              `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks            *bool              `json:"enableServiceLinks,omitempty"`
	TerminationGracePeriodSeconds *int64             `json:"terminationGracePeriodSeconds,omitempty"`
	ServiceAccountName            string             `json:"serviceAccountName,omitempty"`
	RuntimeClassName              *string            `json:"runtimeClassName,omitempty"`
	ImagePullSecrets              []k8sLocalObjRef   `json:"imagePullSecrets,omitempty"`
	SecurityContext               *k8sPodSecurityCtx `json:"securityContext,omitempty"`
	Containers                    []k8sContainer     `json:"containers"`
	Volumes                       []k8sVolume        `json:"volumes,omitempty"`
	NodeSelector                  map[string]string  `json:"nodeSelector,omitempty"`
	Tolerations                   []k8sToleration    `json:"tolerations,omitempty"`
}

type k8sLocalObjRef struct {
	Name string `json:"name"`
}

type k8sToleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

type k8sPodSecurityCtx struct {
	RunAsNonRoot   *bool              `json:"runAsNonRoot,omitempty"`
	RunAsUser      *int64             `json:"runAsUser,omitempty"`
	RunAsGroup     *int64             `json:"runAsGroup,omitempty"`
	FSGroup        *int64             `json:"fsGroup,omitempty"`
	SeccompProfile *k8sSeccompProfile `json:"seccompProfile,omitempty"`
}

type k8sSeccompProfile struct {
	Type             string  `json:"type"`
	LocalhostProfile *string `json:"localhostProfile,omitempty"`
}

type k8sContainer struct {
	Name            string              `json:"name"`
	Image           string              `json:"image"`
	ImagePullPolicy string              `json:"imagePullPolicy,omitempty"`
	Command         []string            `json:"command,omitempty"`
	WorkingDir      string              `json:"workingDir,omitempty"`
	SecurityContext *k8sContainerSecCtx `json:"securityContext,omitempty"`
	Resources       *k8sResources       `json:"resources,omitempty"`
	VolumeMounts    []k8sVolumeMount    `json:"volumeMounts,omitempty"`
}

type k8sContainerSecCtx struct {
	AllowPrivilegeEscalation *bool            `json:"allowPrivilegeEscalation,omitempty"`
	ReadOnlyRootFilesystem   *bool            `json:"readOnlyRootFilesystem,omitempty"`
	Capabilities             *k8sCapabilities `json:"capabilities,omitempty"`
}

type k8sCapabilities struct {
	Drop []string `json:"drop,omitempty"`
}

type k8sResources struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

type k8sVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
	SubPath   string `json:"subPath,omitempty"`
}

type k8sVolume struct {
	Name                  string           `json:"name"`
	EmptyDir              *k8sEmptyDir     `json:"emptyDir,omitempty"`
	PersistentVolumeClaim *k8sPVCVolSource `json:"persistentVolumeClaim,omitempty"`
}

type k8sEmptyDir struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
	Medium    string `json:"medium,omitempty"`
}

type k8sPVCVolSource struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type k8sPodStatus struct {
	Phase             string               `json:"phase,omitempty"`
	Reason            string               `json:"reason,omitempty"`
	Message           string               `json:"message,omitempty"`
	ContainerStatuses []k8sContainerStatus `json:"containerStatuses,omitempty"`
}

type k8sContainerStatus struct {
	Name  string `json:"name"`
	Ready bool   `json:"ready"`
	State struct {
		Waiting *struct {
			Reason  string `json:"reason,omitempty"`
			Message string `json:"message,omitempty"`
		} `json:"waiting,omitempty"`
	} `json:"state,omitempty"`
}

func (c *k8sClient) createPod(ctx context.Context, namespace string, pod *k8sPod) error {
	pod.APIVersion, pod.Kind = "v1", "Pod"
	body, err := json.Marshal(pod)
	if err != nil {
		return fmt.Errorf("marshal pod: %w", err)
	}
	_, err = c.do(ctx, http.MethodPost, "/api/v1/namespaces/"+namespace+"/pods", nil, body)
	return err
}

func (c *k8sClient) getPod(ctx context.Context, namespace, name string) (*k8sPod, error) {
	data, err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+namespace+"/pods/"+name, nil, nil)
	if err != nil {
		return nil, err
	}
	var pod k8sPod
	if err := json.Unmarshal(data, &pod); err != nil {
		return nil, fmt.Errorf("decode pod: %w", err)
	}
	return &pod, nil
}

// deletePod removes a pod immediately (gracePeriodSeconds=0). The sandbox
// image's PID 1 is `sleep`, which has no state worth a graceful drain, and
// the #796 poison path NEEDS the hard kill: a straggler must not get grace
// time to finish a side effect.
func (c *k8sClient) deletePod(ctx context.Context, namespace, name string) error {
	body := []byte(`{"apiVersion":"v1","kind":"DeleteOptions","gracePeriodSeconds":0,"propagationPolicy":"Background"}`)
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/namespaces/"+namespace+"/pods/"+name, nil, body)
	return err
}

type k8sPodList struct {
	Items []k8sPod `json:"items"`
}

func (c *k8sClient) listPods(ctx context.Context, namespace, labelSelector string) (*k8sPodList, error) {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("labelSelector", labelSelector)
	}
	data, err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+namespace+"/pods", q, nil)
	if err != nil {
		return nil, err
	}
	var list k8sPodList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode pod list: %w", err)
	}
	return &list, nil
}

// getNetworkPolicy fetches one networking.k8s.io/v1 NetworkPolicy, used only
// by the boot preflight to verify the sealed-egress policy object exists.
func (c *k8sClient) getNetworkPolicy(ctx context.Context, namespace, name string) error {
	_, err := c.do(ctx, http.MethodGet, "/apis/networking.k8s.io/v1/namespaces/"+namespace+"/networkpolicies/"+name, nil, nil)
	return err
}

// getPVC fetches one PersistentVolumeClaim, used only by the boot preflight to
// verify the shared workspace claim exists before the first pod references it.
func (c *k8sClient) getPVC(ctx context.Context, namespace, name string) error {
	_, err := c.do(ctx, http.MethodGet, "/api/v1/namespaces/"+namespace+"/persistentvolumeclaims/"+name, nil, nil)
	return err
}

// getRuntimeClass fetches one node.k8s.io/v1 RuntimeClass (cluster-scoped),
// used only by the boot preflight when a runtime class is configured.
func (c *k8sClient) getRuntimeClass(ctx context.Context, name string) error {
	_, err := c.do(ctx, http.MethodGet, "/apis/node.k8s.io/v1/runtimeclasses/"+name, nil, nil)
	return err
}

// selfSubjectAccessReview asks the apiserver whether the client's identity can
// perform verb on resource (optionally subresource) in namespace. Used by the
// boot preflight so a missing RBAC grant fails at start with a precise message
// instead of at the first turn.
func (c *k8sClient) selfSubjectAccessReview(ctx context.Context, namespace, verb, resource, subresource string) (bool, error) {
	review := map[string]any{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]any{
			"resourceAttributes": map[string]any{
				"namespace":   namespace,
				"verb":        verb,
				"resource":    resource,
				"subresource": subresource,
				"group":       "",
			},
		},
	}
	body, err := json.Marshal(review)
	if err != nil {
		return false, fmt.Errorf("marshal access review: %w", err)
	}
	data, err := c.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", nil, body)
	if err != nil {
		return false, err
	}
	var resp struct {
		Status struct {
			Allowed bool   `json:"allowed"`
			Reason  string `json:"reason,omitempty"`
		} `json:"status"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, fmt.Errorf("decode access review: %w", err)
	}
	return resp.Status.Allowed, nil
}

// serverVersion fetches /version — the cheapest authenticated "is the
// apiserver reachable and are my credentials valid" probe the preflight runs.
func (c *k8sClient) serverVersion(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/version", nil, nil)
	if err != nil {
		return "", err
	}
	var v struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("decode /version: %w", err)
	}
	return v.GitVersion, nil
}
