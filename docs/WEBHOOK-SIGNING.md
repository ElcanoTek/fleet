# Webhook signing (verifying fleet's outbound webhooks)

fleet's host-side notifier (`internal/notify`, #208) can POST a JSON payload to a
webhook URL when a scheduled task reaches a terminal status. When a signing
secret is configured, fleet signs each delivery with **HMAC-SHA256** so the
receiver can verify the request really came from this fleet and was not replayed
(#316).

This is the same construction GitHub, Stripe, and Shopify use: the receiver holds
a copy of the shared secret and recomputes the MAC over the bytes it received. A
forged or tampered request fails the check.

## Configuration (operator)

Both the URL and the secret come from the host env-file (the operator's
`.env` / systemd `EnvironmentFile`). They are held **host-side only** — never
shipped into the sandbox, the model context, or any log line:

| Variable               | Meaning                                                        |
| ---------------------- | ------------------------------------------------------------- |
| `FLEET_WEBHOOK_URL`    | Where to POST the payload. Unset = the webhook channel is off.|
| `FLEET_WEBHOOK_SECRET` | HMAC signing key. **Unset = deliveries are unsigned.**        |

```ini
FLEET_WEBHOOK_URL=https://hooks.example.com/fleet
FLEET_WEBHOOK_SECRET=<a long random string>
```

Generate a strong secret, e.g. `openssl rand -hex 32`, and share it with the
receiver out of band. Rotating it is a matter of changing the env value on both
sides and restarting fleet.

**Default behavior is unchanged:** with no `FLEET_WEBHOOK_SECRET`, fleet sends the
same unsigned POST it did before #316 — neither signature header is set, and a
receiver that does not verify keeps working.

## The signing scheme (exact bytes)

For each signed delivery fleet sets two headers:

```
X-Fleet-Signature: v1=<hex>
X-Fleet-Timestamp: <unix-seconds>
```

where `<hex>` is computed as:

1. `timestamp` — the request time as Unix seconds, as a decimal string. This is
   the **exact** string sent in `X-Fleet-Timestamp`.
2. `body` — the **exact** raw request body bytes (the JSON payload, byte-for-byte
   as transmitted; do not re-serialize).
3. `signedPayload = timestamp + "." + body` (a literal `.` joins them).
4. `signature = HMAC-SHA256(secret, signedPayload)`, lowercase hex-encoded.
5. The header value is `"v1=" + signature`.

The `v1=` prefix versions the scheme so it can evolve without silently breaking
receivers. The timestamp is **bound into the MAC** (not just sent alongside it),
so an attacker cannot take a captured signature and replay it under a fresh
timestamp — changing the timestamp invalidates the signature.

## Verifying a request (receiver)

A correct verifier must:

1. Read the **raw** request body before any framework re-parses or re-serializes
   it (a re-encoded body will not match byte-for-byte).
2. Recompute the HMAC over `"<X-Fleet-Timestamp>.<raw-body>"` and compare it to
   `X-Fleet-Signature` using a **constant-time** comparison.
3. Reject requests whose timestamp is outside a small **replay window**
   (5 minutes is a good default). fleet cannot enforce this for you — the
   receiver owns validation — but the timestamp is signed so you can trust it.

### Python

```python
import hmac, hashlib, time

def verify_fleet_webhook(raw_body: bytes, timestamp: str, sig_header: str,
                         secret: str, max_skew_seconds: int = 300) -> bool:
    """Return True iff the signature is valid and the timestamp is fresh."""
    # 1. Replay window: reject stale (or future-dated) requests.
    try:
        if abs(time.time() - int(timestamp)) > max_skew_seconds:
            return False
    except (TypeError, ValueError):
        return False
    # 2. Recompute the MAC over the exact signed payload.
    signed = timestamp.encode() + b"." + raw_body
    expected = "v1=" + hmac.new(secret.encode(), signed, hashlib.sha256).hexdigest()
    # 3. Constant-time compare.
    return hmac.compare_digest(expected, sig_header)
```

### Go

```go
func verifyFleetWebhook(rawBody []byte, timestamp, sigHeader, secret string) bool {
    ts, err := strconv.ParseInt(timestamp, 10, 64)
    if err != nil {
        return false
    }
    if d := time.Since(time.Unix(ts, 0)); d < -5*time.Minute || d > 5*time.Minute {
        return false
    }
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(timestamp + "." + string(rawBody)))
    expected := "v1=" + hex.EncodeToString(mac.Sum(nil))
    return hmac.Equal([]byte(expected), []byte(sigHeader))
}
```

## Security notes

- **The secret stays host-side.** Only the derived HMAC ever leaves the fleet
  process (in `X-Fleet-Signature`). The raw secret is never logged, never placed
  in a header, and never enters the sandbox.
- **Use HTTPS.** Signing proves authenticity and integrity, not confidentiality.
  TLS keeps the payload private in transit.
- **Replay protection is the receiver's job.** fleet stamps and signs the
  timestamp; the receiver must reject stale requests within its chosen window.
