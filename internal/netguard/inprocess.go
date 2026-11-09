package netguard

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
)

type inProcessEntry struct {
	name    string
	match   func(*url.URL) bool
	handler http.Handler
}

var (
	inProcessMu      sync.RWMutex
	inProcessEntries []inProcessEntry
)

// RegisterInProcessHandler registers (or replaces, by name) an in-process HTTP
// handler for outbound requests whose URL satisfies match. Passing nil for
// match or handler removes the named entry.
//
// When Fleet hosts a built-in OAuth/MCP surface (such as internal/ms365mcp) on
// its own public hostname, /etc/hosts on Fleet boxes maps that hostname to
// 127.0.0.1. Routing self-targeted requests in-process avoids dialing loopback
// (which IsBlockedIP rightfully refuses for general SSRF defense) while
// preserving full SSRF protection for every other destination.
func RegisterInProcessHandler(name string, match func(*url.URL) bool, handler http.Handler) {
	inProcessMu.Lock()
	defer inProcessMu.Unlock()

	for i := range inProcessEntries {
		if inProcessEntries[i].name == name {
			if match == nil || handler == nil {
				inProcessEntries = append(inProcessEntries[:i], inProcessEntries[i+1:]...)
			} else {
				inProcessEntries[i] = inProcessEntry{name: name, match: match, handler: handler}
			}
			return
		}
	}
	if match != nil && handler != nil {
		inProcessEntries = append(inProcessEntries, inProcessEntry{
			name:    name,
			match:   match,
			handler: handler,
		})
	}
}

func matchInProcessHandler(u *url.URL) http.Handler {
	if u == nil {
		return nil
	}
	inProcessMu.RLock()
	defer inProcessMu.RUnlock()
	for _, entry := range inProcessEntries {
		if entry.match != nil && entry.match(u) {
			return entry.handler
		}
	}
	return nil
}

// WrapRoundTripper wraps base so requests matching a registered in-process
// handler are served directly in memory without dialing the network. All other
// requests delegate to base unchanged.
func WrapRoundTripper(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &inProcessRoundTripper{base: base}
}

type inProcessRoundTripper struct {
	base http.RoundTripper
}

func (rt *inProcessRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req != nil && req.URL != nil {
		if h := matchInProcessHandler(req.URL); h != nil {
			if err := req.Context().Err(); err != nil {
				if req.Body != nil {
					_ = req.Body.Close()
				}
				return nil, err
			}
			if req.Body != nil {
				defer func() { _ = req.Body.Close() }()
			}
			srvReq := req.Clone(req.Context())
			srvReq.RequestURI = req.URL.RequestURI()
			if srvReq.Host == "" {
				srvReq.Host = req.URL.Host
			}
			if srvReq.RemoteAddr == "" {
				srvReq.RemoteAddr = "127.0.0.1:0"
			}
			if srvReq.Body == nil {
				srvReq.Body = http.NoBody
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, srvReq)
			resp := rec.Result()
			resp.Request = req
			return resp, nil
		}
	}
	return rt.base.RoundTrip(req)
}

func (rt *inProcessRoundTripper) CloseIdleConnections() {
	if closer, ok := rt.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}
