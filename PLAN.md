# Mail Relay Service — Implementation Plan

## Context

Build a Go service that accepts inbound emails via SMTP, evaluates them against user-defined rules, and dispatches webhook calls. No UI — configuration is file-based (YAML, JSON, or TOML). This is a greenfield project.

## Project Structure

```
mailrelay/
  cmd/mailrelay/main.go             — entrypoint
  internal/
    models/models.go                — shared types (ParsedEmail, Rule, AppConfig, etc.)
    config/config.go                — config loading via koanf (YAML/JSON/TOML)
    smtp/server.go                  — SMTP server wrapping go-smtp
    smtp/parser.go                  — email parsing (net/mail + mime/multipart)
    smtp/auth.go                    — SPF, DKIM, DMARC, ARC verification
    rules/engine.go                 — rule matching engine
    webhook/dispatcher.go           — HTTP dispatcher with retry
  config.example.yaml               — example config for users
```

## Dependencies

- `github.com/emersion/go-smtp` — embedded SMTP server
- `github.com/knadh/koanf/v2` — multi-format config loading
  - `github.com/knadh/koanf/parsers/yaml`
  - `github.com/knadh/koanf/parsers/json`
  - `github.com/knadh/koanf/parsers/toml`
  - `github.com/knadh/koanf/providers/file`
- `github.com/emersion/go-msgauth` — DKIM, DMARC verification
- `blitiri.com.ar/go/spf` — SPF verification
- Standard library: `net/mail`, `mime/multipart`, `text/template`, `log/slog`, `net/http`

## Implementation Steps

### Step 1: Project init + shared types (`internal/models/models.go`)

- `go mod init` + `go get` dependencies
- Define all structs:
  - `ParsedEmail` — from, to, cc, subject, text/html body, headers, attachments
  - `Attachment` — filename, content type, base64 content
  - `AuthResult` — SPF, DKIM, DMARC, ARC verification results per email
  - `MatcherConfig` — to_email, from_email, subject, from_domain, to_domain (glob/regex patterns)
  - `WebhookConfig` — url, method, headers, payload_template
  - `Rule` — name, match config, webhook config
  - `RetryConfig` — max_retries, initial_wait, max_wait
  - `SMTPConfig` — addr (default `0.0.0.0:25`), domain, max_message_bytes, timeouts, `allowed_recipients` (list of accepted to-address patterns)
  - `AuthConfig` — booleans to enable/enforce SPF, DKIM, DMARC, ARC checks
  - `HealthConfig` — addr
  - `AppConfig` — top-level combining all above
- Helper methods: `FromDomain()`, `ToDomains()` on ParsedEmail

### Step 2: Config loading (`internal/config/config.go`)

- `Load(path string) (*models.AppConfig, error)` using koanf
- Auto-detect parser from file extension (`.yaml`/`.yml` → yaml parser, `.json` → json parser, `.toml` → toml parser)
- Set sensible defaults (SMTP on `0.0.0.0:25`, 3 retries, auth checks enabled but not enforced by default, etc.)
- Validate: at least one rule, each rule has a webhook URL, default method to POST
- Validate Go templates at load time (fail early on bad templates)
- Validate `allowed_recipients` patterns are valid globs

### Step 3: Rule matching engine (`internal/rules/engine.go`)

- `Engine` with `Match(email) []Rule` — returns ALL matching rules
- Glob matching via `path.Match` (case-insensitive — lowercase both sides)
- Optional regex support: patterns wrapped in `/…/` use `regexp.MatchString`
- AND logic within a rule (all non-empty matchers must match)
- OR logic for multi-value fields (any To address matching is sufficient)
- Empty matcher = matches everything

### Step 4: Email parser (`internal/smtp/parser.go`)

- `ParseEmail(r io.Reader) (*models.ParsedEmail, error)`
- Uses `net/mail.ReadMessage` + `mime/multipart.Reader`
- Handle Content-Transfer-Encoding (base64, quoted-printable)
- Extract text body, HTML body, and attachments (base64-encoded)
- Recursive multipart parsing for nested MIME structures

### Step 5: Email authentication (`internal/smtp/auth.go`)

- `VerifyAuth(ctx, remoteAddr, from, emailData) (*models.AuthResult, error)`
- **SPF**: Use `blitiri.com.ar/go/spf` to check the sending IP against the sender domain's SPF record
- **DKIM**: Use `go-msgauth/dkim` to verify DKIM signatures in the email headers
- **DMARC**: Use `go-msgauth/dmarc` to verify alignment between SPF/DKIM and the From header domain
- **ARC**: Check for ARC-Authentication-Results headers as a basic signal (go-msgauth doesn't provide full ARC verification)
- Each check returns pass/fail/none and is recorded in `AuthResult`
- Config controls per-check behavior:
  - `enabled: true` (default) — run the check and include result in webhook payload
  - `enforce: false` (default) — log failures but still accept the email
  - `enforce: true` — reject the email at SMTP level (5xx) if the check fails

### Step 6: SMTP server (`internal/smtp/server.go`)

- Wraps `go-smtp` with `Backend` and `Session` implementations
- Open relay, no SMTP auth, security via bind address + allowed recipients
- **Recipient filtering**: In `session.Rcpt()`, check the recipient address against `smtp.allowed_recipients` patterns. Reject with 550 if no pattern matches. If `allowed_recipients` is empty/unset, accept all.
- On `session.Data`:
  1. Buffer the raw email data (needed for DKIM/ARC verification which requires full message)
  2. Run `VerifyAuth()` with remote IP, envelope sender, raw data
  3. If any enforced auth check fails, return SMTP 550 error
  4. Parse email via `ParseEmail()`
  5. Attach `AuthResult` to `ParsedEmail`
  6. Invoke handler in a goroutine (return 250 OK immediately)
- Uses envelope addresses (MAIL FROM / RCPT TO) as canonical From/To

### Step 7: Webhook dispatcher (`internal/webhook/dispatcher.go`)

- `Dispatcher` with `Dispatch(ctx, email, rules)` — fires all webhooks concurrently via goroutines + WaitGroup
- Exponential backoff retry: configurable max retries (default 3), initial wait (1s), max wait (30s)
- 4xx = not retryable, 5xx = retryable, network errors = retryable
- Payload: JSON-serialized ParsedEmail (including AuthResult) by default, or rendered Go template if `payload_template` is set
- Custom headers per webhook (e.g., Authorization tokens)

### Step 8: Main entrypoint (`cmd/mailrelay/main.go`)

- Parse `-config` flag (default `config.yaml`)
- Load config → create logger (slog JSON) → create rule engine → create dispatcher
- Wire email handler: match rules → dispatch webhooks
- Start SMTP server in goroutine
- Start health check HTTP server (`/healthz` on `127.0.0.1:8080`)
- Graceful shutdown on SIGINT/SIGTERM (30s timeout)

### Step 9: Example config (`config.example.yaml`)

```yaml
log_level: info

smtp:
  addr: "0.0.0.0:25"
  domain: "mailrelay.example.com"
  max_message_bytes: 26214400  # 25MB
  max_recipients: 100
  read_timeout: 60s
  write_timeout: 60s
  allowed_recipients:
    - "*@alerts.example.com"
    - "support@example.com"

auth:
  spf:
    enabled: true
    enforce: false
  dkim:
    enabled: true
    enforce: false
  dmarc:
    enabled: true
    enforce: false
  arc:
    enabled: true
    enforce: false

health:
  addr: "127.0.0.1:8080"

retry:
  max_retries: 3
  initial_wait: 1s
  max_wait: 30s

rules:
  - name: "alerts-to-slack"
    match:
      to_domain: "alerts.example.com"
    webhook:
      url: "https://hooks.slack.com/services/xxx/yyy/zzz"
      method: POST
      headers:
        Content-Type: "application/json"
      payload_template: |
        {"text": "Email from {{.From}}: {{.Subject}}"}

  - name: "support-tickets"
    match:
      to_email: "support@*"
      from_domain: "*.example.com"
    webhook:
      url: "https://api.ticketsystem.com/incoming"
      method: POST
      headers:
        Authorization: "Bearer my-secret-token"
```

## Key Design Decisions

1. **Async handler** — SMTP returns 250 OK immediately; webhook dispatch happens in background goroutine. Failed webhooks don't reject the email.
2. **All-rules-match** — Every matching rule fires, not first-match-wins.
3. **Glob by default, regex opt-in** — `path.Match` for simple patterns, `/regex/` syntax for power users.
4. **No persistent queue (v1)** — In-memory retry only. Crash = lost retries. Future enhancement could add disk persistence.
5. **Envelope > headers** — SMTP envelope addresses are used as canonical From/To since they represent actual delivery intent.
6. **Auth checks: enabled but not enforced by default** — SPF/DKIM/DMARC/ARC results are logged and included in the webhook payload, but don't reject mail unless explicitly configured with `enforce: true`.
7. **Allowed recipients** — Security measure: the SMTP server rejects mail at the `RCPT TO` stage if the recipient doesn't match any configured pattern. Empty list = accept all.

## Verification

1. Build: `go build ./cmd/mailrelay`
2. Run with example config: `./mailrelay -config config.example.yaml`
3. Health check: `curl http://127.0.0.1:8080/healthz`
4. Send test email: `swaks --to test@alerts.example.com --from sender@example.com --server 127.0.0.1:25 --body "Test email"`
5. Verify webhook calls are made (use a tool like https://webhook.site for testing)
6. Test retry behavior by pointing a rule at a temporarily unavailable endpoint
7. Test recipient filtering: `swaks --to blocked@other.com --server 127.0.0.1:25` should be rejected with 550
8. Test auth enforcement: enable `enforce: true` for DKIM and send an unsigned email — should be rejected
