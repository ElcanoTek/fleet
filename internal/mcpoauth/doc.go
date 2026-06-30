// Package mcpoauth implements the client side of the Model Context Protocol
// authorization flow (MCP spec revision 2025-06-18): OAuth 2.1 authorization
// code with mandatory PKCE (S256), RFC 9728 protected-resource-metadata and RFC
// 8414 authorization-server-metadata discovery, RFC 7591 dynamic client
// registration, and RFC 8707 resource-indicator audience binding. It lets a
// fleet user connect a remote (hosted, HTTP/streamable) MCP server and log in to
// it per user, with tokens brokered host-side (#443).
//
// The package is intentionally dependency-light (standard library only): PKCE,
// the authorization URL, and the token-endpoint POSTs are hand-rolled so the RFC
// 8707 resource parameter is carried consistently on authorize, exchange, AND
// refresh — golang.org/x/oauth2's reusable TokenSource cannot pass it on refresh
// — and so token-endpoint errors are classified precisely (invalid_grant →
// re-auth, invalid_target → retry without resource).
//
// SECURITY. Two properties this package is responsible for:
//
//   - SSRF. The server URL is user-supplied, so every outbound request is meant
//     to go through [SafeHTTPClient], whose dialer rejects private / loopback /
//     link-local / metadata addresses at connection time (DNS-rebinding safe)
//     and refuses to follow redirects (a 30x must not relay a bearer to a new
//     origin). This package never picks the client itself — the caller injects
//     it — so the same code is testable against httptest and safe in production.
//   - Identity. [CanonicalResourceURI] is the single normalizer for a server's
//     identity, reused as the DB key, the encryption AAD, the OAuth state record,
//     the RFC 8707 resource value, and the broker routing key. Treating
//     "URL-ish strings" as interchangeable is how a bearer leaks to the wrong
//     resource; there is exactly one canonicalizer.
//
// This package performs no persistence and holds no secrets at rest — it returns
// tokens to its caller (internal/remotemcp), which encrypts and stores them.
package mcpoauth
