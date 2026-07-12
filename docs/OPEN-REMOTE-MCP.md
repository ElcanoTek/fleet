# Open-access remote MCP connections

## Shipped scope

Fleet's hosted MCP directory can label a server `auth: open`. Adding one of
these entries now creates an immediately connected, per-user remote connection:
Fleet does not run OAuth discovery and does not attach an `Authorization`
header to its MCP requests.

This supports public Streamable HTTP servers such as AWS Knowledge, whose MCP
endpoint accepts protocol requests via POST while an ordinary GET redirects to
provider documentation.

## Security boundary

Remote MCP calls still use Fleet's SSRF-safe host-side HTTP client. HTTP
redirects remain disabled; open access is not a reason to let a configured
origin redirect tool-call payloads to another host. Credentials remain
host-side, and open connections have no token or client secret to store or
share.

## Related auth modes

API-key connections — originally deferred from this change — shipped with the
connector-directory onboarding work: the key is sealed host-side with the same
cipher as OAuth tokens and replayed as a static header (see
`docs/CONNECTOR-ONBOARDING.md`). Open entries may also carry a `{placeholder}`
URL when the vendor authenticates via the URL itself (a key or account id as a
query parameter); the directory card's guided form fills it in.

AWS SigV4 signing remains deferred: it requires request-signing plumbing, not
just credential storage, and must not be approximated with user-supplied
headers in the sandbox.
