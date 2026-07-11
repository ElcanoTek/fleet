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

## Deliberately deferred

This change does not add arbitrary API-key entry or AWS SigV4 signing support.
Those authentication modes require explicit host-side credential plumbing and
must not be approximated with user-supplied headers in the sandbox.
