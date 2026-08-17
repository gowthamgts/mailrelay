# Plan: Regex/extraction template functions for webhook payloads

## Context

Today a webhook payload is built in `internal/webhook/dispatcher.go` → `buildPayload()` using Go's
stdlib `text/template` with **no custom functions registered**. Field values are exposed pre-encoded
as JSON via `emailTemplateData()` (the `rawJSON` type), so `{{.TextBody}}` emits a quoted JSON string.
That lets you embed whole fields, but you **cannot extract a substring** — e.g. pull an order ID out
of the body — when constructing the payload.

The user wants per-email extraction/regex logic when building the payload (e.g. "grab `Order #12345`
from the text body"). Decisions confirmed with the user:

- **Mechanism:** add **regex + string helper functions** to the existing `text/template` engine, and
  expose **raw (non-JSON-encoded) field values** so the functions can operate on plain strings. No new
  scripting engine, no new runtime dependency. This is the most idiomatic fit for the codebase.
- **HTML:** no HTML-parsing dependency (goquery/x-net-html). Raw `.Raw.HTMLBody` string is exposed for
  free so regex-on-HTML still works, but no `htmlText`/CSS-selector helpers.
- **UI:** add a "Test payload" tool in the rule editor to render the template against a stored sample
  email before saving.

Why this approach: `text/template` is already sandboxed (no file/network/arbitrary-Go access), and Go's
`regexp` is RE2 (linear time — no ReDoS). So we get extraction power with essentially zero new security
surface, unlike an embedded JS/Lua engine.

## Approach

### 1. New shared template module — `internal/webhook/template.go` (NEW)

Factor the template logic out of `dispatcher.go` into one place so dispatch, validation, and the new
preview endpoint all use the **same** engine:

- `func FuncMap() template.FuncMap` — the function map (below). Exported so `webui` validation/preview
  can register the same funcs. (`webui` already imports `webhook`, so no import cycle.)
- `func Render(email *models.ParsedEmail, tmplStr string) ([]byte, error)` — parse with `FuncMap()` +
  execute against the template data. Single code path for dispatch and preview.
- `func templateData(email *models.ParsedEmail) (map[string]any, error)` — extends today's
  `emailTemplateData`:
  - Keep all existing top-level keys (`From`, `To`, `Subject`, `TextBody`, ... as `rawJSON`) → **fully
    backward compatible**; existing templates keep working unchanged.
  - Add a new key `Raw` holding the `*models.ParsedEmail` itself, so `{{ .Raw.TextBody }}`,
    `{{ .Raw.HTMLBody }}`, `{{ .Raw.Subject }}`, `{{ .Raw.From }}`, `{{ .Raw.Headers }}` etc. yield raw
    Go values (the map value type changes from `rawJSON` to `any`).
- Move `rawJSON` here from `dispatcher.go`.
- Compiled-regex cache: a package-level `sync.Map` keyed by pattern so regexes aren't recompiled per
  email (templates run once per delivered email).

### 2. Function set (pipeline-friendly: subject string is the LAST arg, sprig-style)

Regex (all use the cached compile; invalid pattern → execution error surfaced by Render):
- `regexFind pattern s` → **if the pattern has capture groups, the first non-empty group; else the
  whole match**; `""` on no match. (Matches the order-ID example shown to the user.)
- `regexFindAll pattern s` → `[]string` of all matches (group-1 if grouped, else whole match).
- `regexReplace pattern repl s` → `ReplaceAllString` (supports `$1`).
- `regexMatch pattern s` → `bool`.

Encoding / strings (operate on raw strings):
- `toJson v` (alias `json`) → `json.Marshal` to a string. **Critical** for safely embedding an extracted
  raw string into JSON: `{{ regexFind "..." .Raw.TextBody | toJson }}`.
- `default def v` → `def` when `v` is empty (fallback for no-match).
- `trim` / `trimSpace`, `lower`, `upper`, `split sep s` (→`[]string`), `join sep list`,
  `replace old new s`, `contains substr s`, `hasPrefix`, `hasSuffix`.
- `header email name` → first value of header `name` (convenience over `index .Raw.Headers "X-..."`).

`index`/`slice`/`len`/`printf` are already builtin.

Semantics to document clearly: `{{ .Field }}` = pre-encoded JSON (embed directly, no quotes).
`{{ .Raw.Field }}` = raw string (use inside funcs; pipe through `toJson` when placing into JSON).
No-match returns `""`, not an error → combine with `default` for fallbacks.

### 3. Wire into dispatch — `internal/webhook/dispatcher.go`

- `buildPayload()` delegates to `webhook.Render(email, wh.PayloadTemplate)` (still falls back to
  `json.Marshal(email)` when `PayloadTemplate == ""`). Remove the inlined parse/execute and the local
  `emailTemplateData`/`rawJSON`.

### 4. Validation must register the funcs — `internal/webui/handler.go`

- The save-time validation (~`handler.go:975`, currently `texttemplate.New("validate").Parse(...)`)
  **must** call `.Funcs(webhook.FuncMap())` before `Parse`, otherwise any template using a new func
  fails validation with "function not defined".

### 5. Payload preview/test tool

- **Backend** (`internal/webui/handler.go`): add `handlePreviewPayload` + route
  `POST /rules/preview-payload`. Reads `payload_template` + `sample_email_id` (CSRF as in existing
  handlers, model it on `handleTestWebhook`). Loads the email via `store.GetEmail(ctx, id)` →
  `EmailRecord.ParsedEmail`, calls `webhook.Render(...)`, returns an HTMX partial showing the rendered
  payload (pretty-printed if it parses as JSON) or the error string inline. Populate the sample dropdown
  from `store.ListEmails(...)` (id + subject + from).
- **Frontend** (`internal/webui/templates/rules.html`): in the webhook section add a "Test payload"
  button, a recent-email selector, and a result panel (HTMX). Update the client-side
  `validatePayloadTemplate()` so the new function names aren't flagged (server-side validation is
  authoritative). Refresh the helper hint/examples to show a regex extraction example.

### 6. Docs / scripts (per CLAUDE.md)

- `README.md`: document `.Raw.*` access, the new functions (with the order-ID + amount example), the
  `.Field` vs `.Raw.Field` distinction, and the new test tool.
- `config.example.yaml` / `config.dev.yaml`: **no schema change** — payload templates are per-rule in
  the DB, not in config. Verify there's nothing to add (expected: none).
- `scripts/test-emails.sh`: add an order-confirmation test email whose body contains a predictable
  marker (e.g. `Order #12345`, `Total: $49.99`) so extraction can be exercised end-to-end.

## Critical files

- `internal/webhook/template.go` (NEW) — FuncMap, Render, templateData, regex cache, `rawJSON`.
- `internal/webhook/dispatcher.go` — `buildPayload()` delegates to `Render()`.
- `internal/webhook/template_test.go` (NEW) + `internal/webhook/dispatcher_test.go` — func tests.
- `internal/webui/handler.go` — register `FuncMap()` in validation; add `handlePreviewPayload` + route.
- `internal/webui/templates/rules.html` — test tool UI, validator allowlist, hint/examples.
- `README.md`, `scripts/test-emails.sh`.

## Tests / verification

1. **Unit:** `just test ./internal/webhook/...` — cover: `regexFind` group-vs-whole-match, no-match `""`,
   `regexFindAll`, `regexReplace` with `$1`, `regexMatch`, `toJson` escaping, `default` fallback,
   `.Raw.*` raw access, regex-cache reuse, invalid-pattern error, and an end-to-end `buildPayload` with
   an extraction template producing valid JSON.
2. **Validation parity:** a template using `regexFind` saves successfully (no false "function not
   defined") and a genuinely malformed template is still rejected.
3. **End-to-end:** `just dev`, run `scripts/test-emails.sh` (with the new order email), create a rule
   whose payload template extracts `order_id`/`amount`, point it at the built-in webhook test, and
   confirm the rendered payload in the delivery detail modal.
4. **UI (mandatory per CLAUDE.md):** use the **Browser agent** against http://localhost:2623 to exercise
   the rule editor's "Test payload" tool — pick a sample email, confirm the rendered payload and that
   regex errors/empty matches show inline; check no UI/UX regressions. Delete any screenshots after.

## Out of scope

- Embedded scripting engine (JS/Lua) and HTML-aware extraction (CSS selectors / tag stripping).
- Any config-file schema for extraction (remains per-rule in the DB).
