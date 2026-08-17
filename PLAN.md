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

---

# Plan: Notification Support for Permanent Webhook Delivery Failures

## Context

When mailrelay dispatches webhooks and they permanently fail — either because the endpoint returned a 4xx response (`status = "rejected"`) or all retries were exhausted / timed out (`status = "failed"`) — operators currently have no way to be alerted. This plan adds best-effort async notifications via Email, Slack, and Telegram so operators can react to delivery failures without polling the web UI.

---

## Critical Files

| File | Change |
|---|---|
| `internal/models/models.go` | Add 4 new config structs + field on `AppConfig` |
| `internal/config/config.go` | Add validation for enabled providers |
| `cmd/mailrelay/main.go` | Construct `Notifier`, extend `buildHandler` |
| `internal/notifications/` | New package (5 files) |
| `config.example.yaml` | Document `notifications:` block |
| `config.dev.yaml` | Add disabled stub |
| `README.md` | Document the feature |

---

## Step-by-Step Implementation

### Step 1 — Config structs (`internal/models/models.go`)

Add before `AppConfig`:

```go
type NotificationEmailConfig struct {
    Enabled         bool     `json:"enabled" koanf:"enabled"`
    SMTPAddr        string   `json:"smtp_addr" koanf:"smtp_addr"`
    SMTPUsername    string   `json:"smtp_username" koanf:"smtp_username"`
    SMTPPassword    string   `json:"smtp_password" koanf:"smtp_password"`
    From            string   `json:"from" koanf:"from"`
    To              []string `json:"to,omitempty" koanf:"to"`
    Subject         string   `json:"subject,omitempty" koanf:"subject"`
    MessageTemplate string   `json:"message_template,omitempty" koanf:"message_template"`
}

type NotificationSlackConfig struct {
    Enabled         bool   `json:"enabled" koanf:"enabled"`
    WebhookURL      string `json:"webhook_url" koanf:"webhook_url"`
    MessageTemplate string `json:"message_template,omitempty" koanf:"message_template"`
}

type NotificationTelegramConfig struct {
    Enabled         bool   `json:"enabled" koanf:"enabled"`
    BotToken        string `json:"bot_token" koanf:"bot_token"`
    ChatID          string `json:"chat_id" koanf:"chat_id"`
    ParseMode       string `json:"parse_mode,omitempty" koanf:"parse_mode"` // "HTML" or "MarkdownV2"; default "HTML"
    MessageTemplate string `json:"message_template,omitempty" koanf:"message_template"`
}

type NotificationConfig struct {
    Email    NotificationEmailConfig    `json:"email" koanf:"email"`
    Slack    NotificationSlackConfig    `json:"slack" koanf:"slack"`
    Telegram NotificationTelegramConfig `json:"telegram" koanf:"telegram"`
}
```

Add to `AppConfig`:
```go
Notifications NotificationConfig `json:"notifications" koanf:"notifications"`
```

### Step 2 — Config validation (`internal/config/config.go`)

In the `validate()` function, add checks for enabled providers:
- Email enabled → `smtp_addr`, `from`, and at least one `to` are required
- Slack enabled → `webhook_url` required
- Telegram enabled → `bot_token` and `chat_id` required
- For any provider with a non-empty `message_template` → validate it parses as a `text/template`

### Step 3 — New package `internal/notifications/`

**`templates.go`** — default template constants + `renderTemplate(tmpl *template.Template, data NotificationData) (string, error)`

Default templates:
- `defaultEmailBody` — plain text with all fields
- `defaultSlackTemplate` — Markdown-formatted for Slack
- `defaultTelegramTemplate` — HTML-formatted (for `parse_mode: HTML`)

**`email.go`** — `emailProvider` using `net/smtp.SendMail` (standard library only, no new deps). Templates compiled at construction. TLS-only (port 465) is a known limitation; `SendMail` supports STARTTLS.

**`slack.go`** — `slackProvider` posting `{"text": "..."}` JSON to the configured incoming webhook URL via `net/http`.

**`telegram.go`** — `telegramProvider` calling `https://api.telegram.org/botTOKEN/sendMessage` via `net/http`.

**`notifier.go`** — central type:

```go
type NotificationData struct {
    RuleName     string
    Status       string    // "failed" or "rejected"
    StatusCode   int
    ErrorMessage string
    Attempts     int
    Subject      string
    From         string
    To           []string
    Timestamp    time.Time
}

type Notifier struct { providers []provider }

func NewNotifier(cfg models.NotificationConfig) (*Notifier, error)

// NotifyFailure fires best-effort async notifications.
// Each provider runs in its own goroutine; errors are only logged.
// Uses context.WithoutCancel so the goroutines aren't cancelled when
// the SMTP session context closes.
func (n *Notifier) NotifyFailure(ctx context.Context, email *models.ParsedEmail, result models.DeliveryResult)
```

`NewNotifier` instantiates only the enabled providers; returns a zero-provider notifier (no-op) if all are disabled.

### Step 4 — Wire into `cmd/mailrelay/main.go`

After `config.Load()` (line 37), construct the notifier:
```go
notifier, err := notifications.NewNotifier(cfg.Notifications)
if err != nil {
    fmt.Fprintf(os.Stderr, "failed to initialize notifier: %v\n", err)
    os.Exit(1)
}
```

Extend `buildHandler` signature:
```go
func buildHandler(..., notifier *notifications.Notifier) smtpserver.EmailHandler
```

In the `for _, res := range results` loop (line 268), after saving the delivery record, add:
```go
if res.Status == "failed" || res.Status == "rejected" {
    notifier.NotifyFailure(ctx, email, res)
}
```

Update the call site on line 102:
```go
handler := buildHandler(engine, dispatcher, store, events, cfg.WebUI.RawEmailDir, notifier)
```

Note: `applySettings` (line 447) is intentionally NOT updated — notification config contains credentials and must not be hot-reloaded via the web UI settings page.

### Step 5 — Update config files

**`config.example.yaml`** — add a fully commented `notifications:` section documenting all fields and their `MAILRELAY_NOTIFICATIONS__*` env var equivalents.

**`config.dev.yaml`** — add disabled `notifications:` stub.

**`README.md`** — add a "Failure Notifications" section describing the three providers, required fields, and template variables.

### Step 6 — Tests (`internal/notifications/notifier_test.go`)

- `NewNotifier` with all disabled → no-op notifier, zero providers
- `renderTemplate` with default and custom templates → expected output
- `slackProvider.send` → verify correct JSON body posted to `httptest.NewServer`; non-2xx returns error
- `telegramProvider.send` → verify correct JSON body to mock server
- `emailProvider` → bad template at construction returns error
- `NotifyFailure` with `status = "success"` → no provider called
- `NotifyFailure` with `status = "failed"` → provider goroutine fires

---

## No New Dependencies

All three providers use only the Go standard library (`net/smtp`, `net/http`, `encoding/json`, `text/template`). `go.mod` is unchanged.

---

## Verification

1. Run `go build ./...` — no compile errors
2. Run `go test ./internal/notifications/...` — all tests pass
3. Run `go test ./internal/config/...` — validation tests pass
4. Configure Slack webhook in `config.dev.yaml`, start server with `just dev`, send a test email to a rule whose webhook URL returns 400 → Slack message received
5. Verify `NotifyFailure` does not block the SMTP session by checking delivery timestamps in the web UI
