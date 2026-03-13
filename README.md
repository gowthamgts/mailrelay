<p align="center">
  <img src="assets/logo.png" alt="mailrelay logo" width="120">
</p>

<h1 align="center">mailrelay</h1>

<p align="center">A lightweight Go service that accepts inbound emails via SMTP, matches them against configurable rules, and dispatches webhook calls. Includes an optional web UI for inspecting email history and delivery status.</p>

> **Disclaimer:** This project was generated with the assistance of AI (Claude) and is not suitable for production use. It has not been audited for security, thoroughly tested, or battle-tested under real-world load. Use it for experimentation, learning, or as a starting point — but review and harden the code before deploying it anywhere that matters.

## How it works

```
Incoming Email (SMTP)
        │
        ▼
┌───────────────┐
│  Auth Checks  │──▶ SPF / DKIM / DMARC / ARC
│  (optional)   │
└───────┬───────┘
        │
        ▼
┌───────────────┐
│ Rule Matching │──▶ Glob & regex patterns on from/to/subject/domain
└───────┬───────┘
        │
        ▼
┌───────────────┐
│   Webhooks    │──▶ HTTP POST with JSON payload or custom template
│  (async)      │    Exponential backoff retry on failure
└───────────────┘
```

The SMTP server returns `250 OK` immediately after accepting an email. Webhook dispatch happens asynchronously in background goroutines — failed webhooks never cause email rejection.

## Quick start

### From source

```bash
git clone https://github.com/gowthamgts/mailrelay.git
cd mailrelay
go build ./cmd/mailrelay

cp config.example.yaml config.yaml
# edit config.yaml — enable the web UI, then create rules from the browser

./mailrelay
# or: ./mailrelay -config /path/to/config.yaml
```

### With Docker

The Docker image stores all persistent data under `/data` (database, raw emails, optional config file). The web UI is enabled by default.

```bash
docker run -d \
  --name mailrelay \
  -p 25:25 -p 2623:2623 \
  -v mailrelay_data:/data \
  -e MAILRELAY_SMTP__DOMAIN=mx.example.com \
  ghcr.io/gowthamgts/mailrelay:latest
```

To use a config file instead, mount it at `/data/config.yaml:ro`.

A `docker-compose.example.yaml` is also included in the repo.

## Configuration

Configuration is loaded in layers (highest priority wins):

1. **Hardcoded defaults**
2. **Config file** (optional) — YAML, JSON, or TOML
3. **Environment variables** — `MAILRELAY_` prefix, `__` as nesting delimiter (e.g. `MAILRELAY_SMTP__DOMAIN`)

See [`config.example.yaml`](config.example.yaml) for the full reference with all available options and their defaults.

### Key defaults

| Setting                     | Default          |
| --------------------------- | ---------------- |
| `smtp.addr`                 | `0.0.0.0:25`     |
| `smtp.max_message_bytes`    | 25 MB            |
| `http.addr`                 | `127.0.0.1:2623` |
| `webui.enabled`             | `false`          |
| `webui.retention_days`      | 7                |
| `retry.max_retries`         | 3                |
| `auth.{spf,dkim,dmarc,arc}` | `log`            |

## Rule matching

Rules are created and managed via the web UI at `/rules` and stored in SQLite. Each rule pairs a **matcher** (which emails to act on) with a **webhook** (where to send them). Rules take effect immediately — no restart required.

- **Glob patterns** (default): `support@*`, `*.example.com`, `ALERT*`
- **Regex patterns**: wrap in `/…/` for full regex, e.g. `/^\[URGENT\]/`
- Match fields: To Email, From Email, Subject, To Domain, From Domain
- All non-empty matchers must match (AND logic); omitted fields match everything
- Every matching rule fires its webhook (not first-match-wins)

## Webhook payload

When `payload_template` is omitted, the full parsed email is sent as JSON:

```json
{
  "from": "sender@example.com",
  "to": ["recipient@example.com"],
  "subject": "Hello World",
  "text_body": "Plain text content",
  "html_body": "<p>HTML content</p>",
  "attachments": [{ "filename": "report.pdf", "content_type": "application/pdf", "content": "base64..." }],
  "auth_result": { "spf": "pass", "dkim": "pass", "dmarc": "pass", "arc": "none" },
  "envelope_from": "sender@example.com",
  "envelope_to": ["recipient@example.com"]
}
```

Custom templates use Go's `text/template` syntax with fields like `.From`, `.To`, `.Subject`, `.TextBody`, `.HTMLBody`, `.EnvelopeFrom`, `.EnvelopeTo`, `.Attachments`, and `.AuthResult`.

## Email authentication

SPF, DKIM, DMARC, and ARC are verified for every inbound email. Each check can be set to `off`, `log` (default), or `enforce` (rejects on failure with SMTP 550).

## Web UI

Enable with `webui.enabled: true`. Served at `http://<http.addr>/`.

- Create, edit, enable/disable, and delete rules
- Browse and search received emails
- View email details, headers, body (text/HTML), and auth results
- Download attachments and raw `.eml` files (when `raw_email_dir` is set)
- Inspect webhook delivery outcomes and replay failed deliveries
- Real-time updates via SSE
- Runtime settings management at `/settings`
- Dark mode support

## Health check & metrics

| Endpoint   | Description               |
| ---------- | ------------------------- |
| `/healthz` | Liveness probe — returns `ok` |
| `/metrics` | Prometheus metrics (`mailrelay_*` prefix) |

See [docs/prometheus.md](docs/prometheus.md) for the full list of exposed metrics. A ready-to-import [Grafana dashboard](docs/grafana-dashboard.json) is also included.

## Testing

```bash
# Install swaks: brew install swaks (macOS) or apt install swaks (Ubuntu)
swaks --to test@alerts.example.com \
      --from sender@example.com \
      --server 127.0.0.1:25 \
      --header "Subject: Test Alert" \
      --body "Test email body"
```

## License

MIT
