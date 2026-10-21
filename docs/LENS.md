# Lens jobs from fleet (#1506)

Users can ask the agent, in chat or in a scheduled task, to classify a list
of websites, iOS/Android apps or CTV apps with Elcano Lens, check on the job,
and pull the results into the workspace. This note records how that is built
and why **fleet itself has no Lens code**.

## Shape

Lens is an external service with its own MCP server. Each client bundle
declares it as an ordinary HTTP connector, the same way bundles declare
Pages:

| Piece | Lives in | What it is |
|---|---|---|
| `POST /mcp`, `/api/v1`, scoped tokens | `ElcanoTek/lens` (`lens_mcp.py`, `lens_api.py`, `api_tokens.py`; contract in its `docs/API.md`) | A stateless Streamable-HTTP MCP server with JSON responses. Tools: `create_job`, `get_job`, `list_jobs`, `get_job_results`, `get_download_link`, `cancel_job`, `list_input_files`, `get_options` |
| `lens` connector | each `*-config` bundle's `manifest.yaml` | `type: http`, `url: ${LENS_MCP_URL:-https://lens.invalid/mcp}`, a bearer `LENS_API_TOKEN` header, `enabled_env: [LENS_MCP_URL, LENS_API_TOKEN]` |
| Approval policy | the same `agent_policy` | `mcp_lens_create_job` and `mcp_lens_cancel_job` are critical in the default `approve` mode (a started job's OpenRouter spend can't be refunded, so `notify` would not have the complete undo it requires); scheduled runs pass the usual `confirm_audit` gate. The six reads are `parallel_safe_tools` |
| Workflow guidance | `skills/lens-classification/` in each bundle | When to send `items` or a file, polling without duplicate jobs, reading the summary, downloading the CSV |

This follows the coupling doctrine in `AGENTS.md`: bundles are data, fleet is
engine. A deployment without Lens loads the same binary and never sees the
connector.

## The two engine seams it relies on

Both already existed and are generic. Nothing was added for Lens.

- **Files in:** [MCP workspace files](MCP-WORKSPACE-FILES.md). Lens declares
  `create_job.content_base64` as `type: string, contentEncoding: base64`, so
  fleet offers the model a `{workspace_file, sha256, offset, length}`
  reference. Fleet reads the file through the sandbox FileOp seam and
  base64-encodes it in the broker, so the list reaches Lens without its bytes
  entering the model's context. Fleet sends at most 8 MiB per call (from a
  file of up to 64 MiB). `create_job` takes the list in one call, so a list
  can be up to 8 MiB. The tool's `maxLength` and Lens's own request limits
  still apply. Short
  lists go inline as `items`. Bigger files go through the Lens dashboard and
  are then named with `input_file`.
- **Files out:** `download_url`. `get_download_link` returns a 15-minute,
  single-file signed URL that needs no credential, and `download_url` lands
  the CSV in the workspace through the SSRF-guarded host fetch. The bearer
  token never leaves the broker. The signed link does pass through the model's
  context, so it is deliberately short-lived, scoped to one file of one job,
  and invalidated when its token is revoked. That exposure is deliberate: the
  link fetches one file the agent is about to read anyway, the Lens bearer
  token itself never leaves the broker, and the link carries its signature in
  the URL path, not a `token=` parameter, so `governToolOutput`'s redactor
  leaves it intact (checked against `internal/redact` with a real-shaped
  link). A broker-side vault for arbitrary MCP download URLs, like Fast.io's
  `fleet-download://` handle, would remove even that exposure; it is not
  built. **The link must resolve to a
  public address.** `download_url` goes through the SSRF-guarded dialer, which
  refuses loopback, RFC 1918 and link-local targets, so a Lens reachable only
  on a private network can take jobs but not hand back files. Lens builds the
  link from its `LENS_PUBLIC_URL` (or the `Host` it was called on), so point
  `LENS_MCP_URL` at the Lens's public hostname. Every current deployment does
  (`lens.<deployment domain>`, public DNS). A private-only Lens would need a
  brokered download path; that is not built.

## Credentials and scope

`LENS_API_TOKEN` is minted on the Lens host (`lens token create --label
fleet-<deployment>`) with scopes `jobs:read` and `jobs:run`. It is not an
admin key: it cannot delete runs or files, manage tokens, or change settings.
Lens stores only its hash. Fleet brokers it host-side like every other
connector credential.

There is no production default URL. Each deployment points at its own Lens,
and an unset `LENS_MCP_URL` leaves the connector dark rather than falling
back to another organisation's Lens.

## Verified

- Fleet's own `internal/mcp` HTTP client was run against a live Lens built
  from the Lens PR. The handshake succeeded, all eight tools were listed,
  `create_job` queued a job tagged with the token label, and an unknown job id
  came back as a tool result with `isError` and `code: job_not_found`, not as
  a transport failure.
- Each bundle's `mcp/tests/test_lens_connector.py` pins the URL, the gate,
  the tool catalog and the policy.

## Deferred

- No push notification when a Lens job finishes. The agent polls `get_job`,
  sleeping in bash between checks in scheduled runs.
- No list uploads larger than 8 MiB through the agent, Fleet's per-call
  workspace-file maximum (or less, if Lens's own limit is lower).
- No file download from a Lens that only resolves to a private address (see
  "Files out").
- No broker-side vaulting of the signed download link (a generic opaque
  handle like `fleet-download://` for any MCP server's download URL).
- No per-user job isolation inside one Lens instance (the Lens dashboard has
  none either); customers are separated by running separate Lens hosts.
