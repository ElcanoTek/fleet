# Chat-surface metrics: rate-limit rejections and stream reattaches

Two Prometheus counter families on `/metrics` (admin-API-key gated, like every
other family there) report what the chat HTTP surface does under load and
across reconnects. Shipped with #1951.

| Family | Labels | Incremented when |
|---|---|---|
| `fleet_chat_rate_limited_total` | `reason` = `rpm` \| `day` \| `concurrent` | a chat or attachment request is answered **429** by the per-user limiter. `rpm` and `day` are the sliding windows (`FLEET_CHAT_RATE_LIMIT_*`); `concurrent` is the in-flight-turn cap. Admins are exempt and never counted. |
| `fleet_sse_reconnects_total` | `outcome` = `within_buffer` \| `db_fallback` \| `buffer_expired` \| `no_content` | a client reattaches to a conversation's stream (`GET /conversations/{id}/stream`): served from the live turn buffer, replayed from the database for a still-running turn, replayed from the database for a finished turn, or answered 204 because there was nothing to replay. |

Both label sets are fixed, so neither family can grow with users, conversations
or turns. The user and conversation of a given event are in the log line, not
the metric.

## What changed, and what did not

Before #1951 the same events were tallied in two in-memory maps on the HTTP
server (`rateHitCounter`, `reconnectCounter`) that nothing in production read;
their comment still said a Prometheus surface was deferred to #176, which had
long since shipped. The maps, their snapshot accessors and the server fields are
gone; the events, their classification and the HTTP responses are unchanged.

Not done: no per-endpoint label (the limiter guards `/chat` and `/attachments`
together, and splitting the count adds a series for little operator value), and
no gauge of currently-attached streams.
