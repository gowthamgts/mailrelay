# Webhook recipes

How payloads and templates work is covered in the [README](../README.md#payload). This page covers protecting the services that receive them, and ready-made templates for common destinations.

## What mailrelay sends

- Method `POST` by default (you can pick another per rule).
- `Content-Type: application/json`, unless you set your own in the rule's headers.
- `User-Agent: mailrelay/<version>`.
- Any headers you add in the rule's **Headers** box, one per line as `Key: Value`.

A request counts as delivered on any 2xx response. A 4xx response isn't retried, since sending the same payload again won't help. 5xx responses, connection errors and timeouts are retried with exponential backoff (`retry.*` in the config, or per rule).

## Securing your receivers

Set a **Signing secret** in each rule to enable HMAC-SHA256 signing. **Generate**
creates 32 random bytes encoded as hex; copy the displayed value before saving.
Use that exact text as the receiver's secret (do not hex-decode it). Saved secrets
are never sent back to the editor: an empty edit keeps the current secret; use
**Clear saved secret** to disable signing. Secrets are stored in the rule database,
so protect database files and backups. Rotate by entering or generating a replacement.

The pending-delivery queue never stores the secret. It is read from the rule at
send time, so a rotated secret also applies to deliveries that are still retrying;
update your receiver before rotating, or accept failed verifications for those
retries. If the rule is deleted or its secret is cleared while a signed delivery
is pending, that delivery fails with "signing secret … is no longer available"
instead of being sent unsigned.

Signed requests include:

- `X-Mailrelay-Timestamp`: Unix seconds, recalculated for every attempt.
- `X-Mailrelay-Signature`: `sha256=` followed by the lowercase hex HMAC-SHA256 of
  `<timestamp>.<raw body>`, using the literal UTF-8 secret as the key.
- `X-Mailrelay-Delivery`: a UUID stable across automatic retries of one dispatch.
  A replay or editor Test gets a new ID. This ID is not part of the signature;
  only use it for deduplication after verifying the signature, over HTTPS.

The body is the exact default JSON or rendered template bytes sent over HTTP.
Retries use the current timestamp; attempts in the same second can share a
signature. Signing headers override custom headers with the same names. The
editor's **Test** sends `{}` signed with the entered secret or the saved secret
when the field is blank. Clearing the secret also makes Test unsigned.
Signed requests do not follow HTTP redirects; configure the final receiver URL.
An empty secret leaves the existing unsigned behavior unchanged.

Other receiver protections:

- **A secret header.** Add `Authorization: Bearer <token>` or `X-Webhook-Secret: <token>` in the rule's Headers box, and reject requests without it.
- **A secret in the URL.** Many services already work this way (Discord, Slack, Home Assistant and n8n webhook URLs contain a random ID). Treat those URLs as passwords.
- **HTTPS** for anything that leaves your network, so the secret and the email content aren't sent in the clear.

Don't rely on the `User-Agent`; anyone can copy it.

## Verifying signatures

Read the raw body before parsing JSON. Require an integer timestamp within five
minutes in either direction, and compare the expected signature in constant time.
Reject missing or malformed headers. Keep sender and receiver clocks synchronized.
For a captured request you can also use `scripts/verify-webhook.py`: set
`MAILRELAY_SIGNING_SECRET` and pass the timestamp and signature as arguments,
with the exact body on stdin.

These functions take the literal shared secret, raw request bytes, and the two
header values; return an unauthorized response when verification fails.

### Go

```go
import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "strconv"
    "strings"
    "time"
)

func verify(secret string, body []byte, timestamp, signature string) bool {
    seconds, err := strconv.ParseInt(timestamp, 10, 64)
    now := time.Now().Unix()
    if err != nil || seconds < now-300 || seconds > now+300 {
        return false
    }
    if !strings.HasPrefix(signature, "sha256=") {
        return false
    }
    received, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
    if err != nil { return false }
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(timestamp + "."))
    mac.Write(body)
    return hmac.Equal(mac.Sum(nil), received)
}
```

### Python

```python
import hashlib
import hmac
import re
import time


def verify(secret: str, body: bytes, timestamp: str, signature: str) -> bool:
    if not re.fullmatch(r"[0-9]+", timestamp):
        return False
    if abs(int(time.time()) - int(timestamp)) > 300:
        return False
    if not re.fullmatch(r"sha256=[0-9a-f]{64}", signature):
        return False
    expected = "sha256=" + hmac.new(
        secret.encode("utf-8"), timestamp.encode("ascii") + b"." + body,
        hashlib.sha256,
    ).hexdigest()
    return hmac.compare_digest(expected, signature)
```

### Node.js

```js
import { createHmac, timingSafeEqual } from 'node:crypto';

function verify(secret, body, timestamp, signature) {
  // body must be the raw Buffer, captured before JSON middleware.
  if (!/^[0-9]+$/.test(timestamp ?? '')) return false;
  const seconds = Number(timestamp);
  if (!Number.isSafeInteger(seconds) ||
      Math.abs(Math.floor(Date.now() / 1000) - seconds) > 300) return false;
  if (!/^sha256=[0-9a-f]{64}$/.test(signature ?? '')) return false;
  const expected = createHmac('sha256', Buffer.from(secret, 'utf8'))
    .update(timestamp + '.', 'utf8').update(body).digest();
  const received = Buffer.from(signature.slice(7), 'hex');
  return received.length === expected.length && timingSafeEqual(expected, received);
}
```

## Templates

Template variables are already JSON-encoded, so put them in **without quotes**. Each variable is one JSON value, so you can't join two of them into one string; use a separate field for each.

### ntfy

URL: `https://ntfy.sh/` (or your own server). Add `Authorization: Bearer <token>` if your topic is protected.

```
{"topic": "mail-alerts", "title": {{ .Subject }}, "message": {{ .TextBody | default .HTMLBody }}}
```

### Discord

URL: the channel's webhook URL (Channel settings → Integrations → Webhooks).

```
{"embeds": [{"title": {{ .Subject }}, "description": {{ .TextBody }}, "author": {"name": {{ .From }}}}]}
```

Discord rejects embed descriptions over 4096 characters. For long emails, send only the subject, or point the webhook at n8n first and trim it there.

### Slack

URL: an incoming webhook URL from a Slack app.

```
{"text": {{ .Subject }}}
```

### Home Assistant

URL: `https://homeassistant.local:8123/api/webhook/<webhook_id>`. No template is needed; the default payload works. In an automation with a webhook trigger, read the fields as `trigger.json.subject`, `trigger.json.from`, `trigger.json.text_body` and so on.

Home Assistant on your LAN has a private IP, so you also need `webhook.allow_private_networks: true`.

### n8n, Node-RED and other automation tools

Use a Webhook trigger node and leave the template empty. The full payload, including attachments as base64, is the easiest thing to work with there, and you can reshape it in the flow.

### Paperless-ngx

Paperless's upload endpoint wants a multipart file upload, which mailrelay can't send directly. Put n8n (or a small script) in between: receive the default payload, decode `attachments[].content` from base64, and post each file to `/api/documents/post_document/`.

## Testing

- The **Test** button in the rule editor checks that the URL is reachable.
- **Replay** on any email runs it through the current rules again, and the email's detail view shows each response body.
- Private and internal addresses (localhost, your LAN, Docker networks, Tailscale) are blocked unless `webhook.allow_private_networks` is on. A blocked request fails right away, without retries, and its error names the setting.
