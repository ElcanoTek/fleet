package netguard

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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
// handler are served directly in memory via a streaming pipe without dialing
// the network or buffering the response body. All other requests delegate to
// base unchanged.
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

			pr, pw := io.Pipe()
			sw := &streamingResponseWriter{
				header:    make(http.Header),
				pw:        pw,
				committed: make(chan struct{}),
			}

			go func() {
				defer func() {
					if req.Body != nil {
						_ = req.Body.Close()
					}
					sw.commit(http.StatusOK)
					_ = pw.Close()
				}()
				h.ServeHTTP(sw, srvReq)
			}()

			select {
			case <-sw.committed:
				return &http.Response{
					StatusCode:    sw.code,
					Status:        fmt.Sprintf("%d %s", sw.code, http.StatusText(sw.code)),
					Proto:         "HTTP/1.1",
					ProtoMajor:    1,
					ProtoMinor:    1,
					Header:        sw.snapHeader,
					Body:          pr,
					ContentLength: parseInProcessContentLength(sw.snapHeader.Get("Content-Length")),
					Request:       req,
				}, nil
			case <-req.Context().Done():
				_ = pr.CloseWithError(req.Context().Err())
				return nil, req.Context().Err()
			}
		}
	}
	return rt.base.RoundTrip(req)
}

func (rt *inProcessRoundTripper) CloseIdleConnections() {
	if closer, ok := rt.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

type streamingResponseWriter struct {
	header     http.Header
	snapHeader http.Header
	pw         *io.PipeWriter
	code       int
	once       sync.Once
	committed  chan struct{}
}

func (w *streamingResponseWriter) Header() http.Header {
	return w.header
}

func (w *streamingResponseWriter) WriteHeader(statusCode int) {
	w.commit(statusCode)
}

func (w *streamingResponseWriter) Write(p []byte) (int, error) {
	w.commit(http.StatusOK)
	return w.pw.Write(p)
}

func (w *streamingResponseWriter) Flush() {
	w.commit(http.StatusOK)
}

func (w *streamingResponseWriter) commit(statusCode int) {
	w.once.Do(func() {
		if statusCode <= 0 {
			statusCode = http.StatusOK
		}
		w.code = statusCode
		w.snapHeader = w.header.Clone()
		close(w.committed)
	})
}

func parseInProcessContentLength(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}
