package webui

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
	smtpserver "github.com/gowthamgts/mailrelay/internal/smtp"
	"github.com/gowthamgts/mailrelay/internal/storage"
	"github.com/gowthamgts/mailrelay/internal/webhook"
)

const rawEmail = "From: sender@example.com\r\nTo: rcpt@example.com\r\nSubject: Reparsed subject\r\n\r\nHello there\r\n"

type testEnv struct {
	h      *Handler
	store  *storage.Store
	mux    *http.ServeMux
	rawDir string
	dbPath string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	rawDir := filepath.Join(dir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &models.AppConfig{
		LogLevel: "info",
		Auth: models.AuthConfig{
			SPF:   models.AuthModeOff,
			DKIM:  models.AuthModeOff,
			DMARC: models.AuthModeOff,
		},
		Retry: models.RetryConfig{
			MaxRetries:  0,
			InitialWait: time.Second,
			MaxWait:     time.Second,
			Timeout:     5 * time.Second,
		},
	}
	dispatcher := webhook.NewDispatcher(cfg.Retry, "test")
	if err := dispatcher.StartQueue(store, 2, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dispatcher.Shutdown(context.Background()) })
	h, err := NewHandler(store, rules.NewEngine(), dispatcher, rawDir, cfg)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}
	mux := http.NewServeMux()
	h.Register(mux)
	return &testEnv{h: h, store: store, mux: mux, rawDir: rawDir, dbPath: dbPath}
}

func (e *testEnv) do(t *testing.T, method, target string, form url.Values, htmx bool) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

func (e *testEnv) saveEmail(t *testing.T, subject string, status models.EmailStatus, atts ...models.Attachment) string {
	t.Helper()
	rec := &models.EmailRecord{
		EnvelopeFrom: "sender@example.com",
		EnvelopeTo:   []string{"rcpt@example.com"},
		HeaderFrom:   "Sender <sender@example.com>",
		HeaderTo:     []string{"rcpt@example.com"},
		Subject:      subject,
		Status:       status,
		ParsedEmail: &models.ParsedEmail{
			From:         "sender@example.com",
			To:           []string{"rcpt@example.com"},
			Subject:      subject,
			TextBody:     "body of " + subject,
			Attachments:  atts,
			EnvelopeFrom: "sender@example.com",
			EnvelopeTo:   []string{"rcpt@example.com"},
		},
	}
	if err := e.store.SaveEmail(context.Background(), rec); err != nil {
		t.Fatalf("save email: %v", err)
	}
	return rec.ID
}

func (e *testEnv) saveRule(t *testing.T, name, webhookURL string) *models.RuleRecord {
	t.Helper()
	rule := &models.RuleRecord{
		Name:    name,
		Enabled: true,
		Webhook: models.WebhookConfig{URL: webhookURL, Method: "POST"},
	}
	if err := e.store.SaveRule(context.Background(), rule); err != nil {
		t.Fatalf("save rule: %v", err)
	}
	if err := e.h.LoadRules(context.Background()); err != nil {
		t.Fatalf("load rules: %v", err)
	}
	return rule
}

func TestListPage(t *testing.T) {
	e := newTestEnv(t)
	e.saveEmail(t, "First message", models.EmailStatusDelivered)
	e.saveEmail(t, "Second message", models.EmailStatusFailed)

	rec := e.do(t, "GET", "/", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"First message", "Second message", "<html"} {
		if !strings.Contains(body, want) {
			t.Errorf("list page missing %q", want)
		}
	}

	rec = e.do(t, "GET", "/?status=failed&q=Second&page=0", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("partial status = %d", rec.Code)
	}
	body = rec.Body.String()
	if !strings.Contains(body, "Second message") || strings.Contains(body, "First message") {
		t.Errorf("filtered partial has wrong emails")
	}
	if strings.Contains(body, "<html") {
		t.Errorf("htmx request should render the partial")
	}
}

func TestDetail(t *testing.T) {
	e := newTestEnv(t)
	id := e.saveEmail(t, "Detail subject", models.EmailStatusDelivered)
	if err := e.store.SaveDelivery(context.Background(), &models.DeliveryRecord{
		EmailID: id, RuleName: "my-rule", Status: "success", StatusCode: 200, Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.rawDir, id+".eml"), []byte(rawEmail), 0o644)

	rec := e.do(t, "GET", "/emails/"+id, nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{"Detail subject", "my-rule"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("detail missing %q", want)
		}
	}

	rec = e.do(t, "GET", "/emails/"+id, nil, false)
	if rec.Code != http.StatusFound {
		t.Errorf("non-htmx detail status = %d, want redirect", rec.Code)
	}

	rec = e.do(t, "GET", "/emails/missing", nil, true)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing detail status = %d", rec.Code)
	}
}

func TestReplay(t *testing.T) {
	e := newTestEnv(t)

	t.Run("no matching rule drops", func(t *testing.T) {
		id := e.saveEmail(t, "Dropped", models.EmailStatusReceived)
		rec := e.do(t, "POST", "/emails/"+id+"/replay", nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if rec.Header().Get("HX-Trigger") != "refreshList" {
			t.Errorf("missing HX-Trigger")
		}
		got, _ := e.store.GetEmail(context.Background(), id)
		if got.Status != models.EmailStatusDropped {
			t.Errorf("status = %s, want dropped", got.Status)
		}
	})

	t.Run("delivers and reparses raw email", func(t *testing.T) {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Write([]byte("ok"))
		}))
		defer srv.Close()
		e.saveRule(t, "replay-rule", srv.URL)

		id := e.saveEmail(t, "Original subject", models.EmailStatusFailed)
		os.WriteFile(filepath.Join(e.rawDir, id+".eml"), []byte(rawEmail), 0o644)

		rec := e.do(t, "POST", "/emails/"+id+"/replay", nil, true)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			email, _ := e.store.GetEmail(context.Background(), id)
			if email.Status == models.EmailStatusDelivered {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if hits.Load() != 1 {
			t.Errorf("webhook hits = %d, want 1", hits.Load())
		}
		got, _ := e.store.GetEmail(context.Background(), id)
		if got.Status != models.EmailStatusDelivered {
			t.Errorf("status = %s, want delivered", got.Status)
		}
		if got.ParsedEmail.Subject != "Reparsed subject" {
			t.Errorf("subject = %q, want reparsed", got.ParsedEmail.Subject)
		}
		deliveries, _ := e.store.GetDeliveriesForEmail(context.Background(), id)
		if len(deliveries) != 1 || deliveries[0].RuleName != "replay-rule" {
			t.Errorf("deliveries = %+v", deliveries)
		}
	})

	t.Run("missing email", func(t *testing.T) {
		rec := e.do(t, "POST", "/emails/missing/replay", nil, true)
		if rec.Code != http.StatusNotFound {
			t.Errorf("status = %d", rec.Code)
		}
	})
}

func TestDelete(t *testing.T) {
	e := newTestEnv(t)
	id := e.saveEmail(t, "To delete", models.EmailStatusDelivered)
	emlPath := filepath.Join(e.rawDir, id+".eml")
	os.WriteFile(emlPath, []byte(rawEmail), 0o644)

	rec := e.do(t, "DELETE", "/emails/"+id, nil, true)
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Trigger") != "emailDeleted" {
		t.Fatalf("status = %d, trigger = %q", rec.Code, rec.Header().Get("HX-Trigger"))
	}
	if _, err := e.store.GetEmail(context.Background(), id); err == nil {
		t.Errorf("email still exists")
	}
	if _, err := os.Stat(emlPath); !os.IsNotExist(err) {
		t.Errorf("raw email not removed")
	}
}

func TestDeleteAll(t *testing.T) {
	e := newTestEnv(t)
	for _, s := range []string{"a", "b", "c"} {
		id := e.saveEmail(t, s, models.EmailStatusDelivered)
		os.WriteFile(filepath.Join(e.rawDir, id+".eml"), []byte(rawEmail), 0o644)
	}

	rec := e.do(t, "POST", "/emails/delete-all", nil, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	ids, _ := e.store.AllEmailIDs(context.Background())
	if len(ids) != 0 {
		t.Errorf("%d emails left", len(ids))
	}
	entries, _ := os.ReadDir(e.rawDir)
	if len(entries) != 0 {
		t.Errorf("%d raw files left", len(entries))
	}
}

func TestRaw(t *testing.T) {
	e := newTestEnv(t)
	id := e.saveEmail(t, "Raw", models.EmailStatusDelivered)
	os.WriteFile(filepath.Join(e.rawDir, id+".eml"), []byte(rawEmail), 0o644)

	rec := e.do(t, "GET", "/emails/"+id+"/raw", nil, false)
	if rec.Code != http.StatusOK || rec.Body.String() != rawEmail {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}

	rec = e.do(t, "GET", "/emails/"+id+"/raw?download=1", nil, false)
	if ct := rec.Header().Get("Content-Type"); ct != "message/rfc822" {
		t.Errorf("download content type = %q", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, id+".eml") {
		t.Errorf("content disposition = %q", cd)
	}

	for _, target := range []string{"/emails/missing/raw", "/emails/..%2Fsecret/raw"} {
		if rec := e.do(t, "GET", target, nil, false); rec.Code != http.StatusNotFound {
			t.Errorf("%s status = %d", target, rec.Code)
		}
	}

	e.h.rawEmailDir = ""
	if rec := e.do(t, "GET", "/emails/"+id+"/raw", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("no raw dir status = %d", rec.Code)
	}
}

func TestAttachment(t *testing.T) {
	e := newTestEnv(t)
	id := e.saveEmail(t, "Attachments", models.EmailStatusDelivered,
		models.Attachment{Filename: "hello.txt", ContentType: "text/plain", Content: base64.StdEncoding.EncodeToString([]byte("hello"))},
		models.Attachment{ContentType: "application/octet-stream", Content: base64.StdEncoding.EncodeToString([]byte{1, 2})},
		models.Attachment{Filename: "bad.bin", ContentType: "application/octet-stream", Content: "!!not base64!!"},
	)

	rec := e.do(t, "GET", "/emails/"+id+"/attachments/0", nil, false)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "hello.txt") {
		t.Errorf("content disposition = %q", cd)
	}

	rec = e.do(t, "GET", "/emails/"+id+"/attachments/1", nil, false)
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment-1") {
		t.Errorf("unnamed attachment disposition = %q", cd)
	}

	rec = e.do(t, "GET", "/emails/"+id+"/attachments/2", nil, false)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("bad base64 status = %d", rec.Code)
	}

	for _, idx := range []string{"3", "-1", "x"} {
		if rec := e.do(t, "GET", "/emails/"+id+"/attachments/"+idx, nil, false); rec.Code != http.StatusNotFound {
			t.Errorf("index %s status = %d", idx, rec.Code)
		}
	}
	if rec := e.do(t, "GET", "/emails/missing/attachments/0", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("missing email status = %d", rec.Code)
	}
}

func TestStats(t *testing.T) {
	e := newTestEnv(t)
	e.saveEmail(t, "a", models.EmailStatusDelivered)
	e.saveEmail(t, "b", models.EmailStatusDropped)
	e.saveEmail(t, "c", models.EmailStatusFailed)
	e.saveEmail(t, "d", models.EmailStatusPartialFailure)
	e.saveEmail(t, "e", models.EmailStatusRejected)

	rec := e.do(t, "GET", "/api/stats", nil, false)
	var st storage.Stats
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if st.Total != 5 || st.Delivered != 1 || st.Dropped != 1 || st.Failed != 1 || st.PartialFailure != 1 || st.Rejected != 1 {
		t.Errorf("stats = %+v", st)
	}

	rec = e.do(t, "GET", "/api/stats-html?status=failed", nil, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Delivered") {
		t.Errorf("stats html status = %d", rec.Code)
	}
}

func TestSSE(t *testing.T) {
	e := newTestEnv(t)
	srv := httptest.NewServer(e.mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content type = %q", ct)
	}

	r := bufio.NewReader(resp.Body)
	line, _ := r.ReadString('\n')
	if line != ": connected\n" {
		t.Fatalf("first line = %q", line)
	}
	r.ReadString('\n')

	e.h.Events.Notify()
	line, _ = r.ReadString('\n')
	if line != "event: refresh\n" {
		t.Errorf("event line = %q", line)
	}

	// A recorder doesn't implement http.Flusher.
	rec := httptest.NewRecorder()
	e.h.handleSSE(noFlushWriter{rec}, httptest.NewRequest("GET", "/api/events", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("no flusher status = %d", rec.Code)
	}
}

// noFlushWriter hides the recorder's Flush method.
type noFlushWriter struct{ w http.ResponseWriter }

func (n noFlushWriter) Header() http.Header         { return n.w.Header() }
func (n noFlushWriter) Write(b []byte) (int, error) { return n.w.Write(b) }
func (n noFlushWriter) WriteHeader(code int)        { n.w.WriteHeader(code) }

func validSettings() url.Values {
	return url.Values{
		"log_level":                  {"debug"},
		"auth_spf":                   {"log"},
		"auth_dkim":                  {"enforce"},
		"auth_dmarc":                 {"off"},
		"retry_max_retries":          {"3"},
		"retry_initial_wait_seconds": {"2"},
		"retry_max_wait_seconds":     {"30"},
		"retry_timeout_seconds":      {"10"},
		"retry_on_timeout":           {"1"},
		"retention_days":             {"14"},
	}
}

func TestSettings(t *testing.T) {
	e := newTestEnv(t)

	rec := e.do(t, "GET", "/settings?saved=1", nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	lv := new(slog.LevelVar)
	var retention atomic.Int32
	e.h.SetLogLevel(lv)
	e.h.SetRetentionDays(&retention)
	e.h.SetSMTPServer(smtpserver.NewServer(context.Background(), models.SMTPConfig{Addr: "127.0.0.1:0"}, e.h.config.Auth, nil))

	for _, level := range []string{"debug", "info", "warn", "error"} {
		form := validSettings()
		form.Set("log_level", level)
		rec = e.do(t, "POST", "/settings", form, false)
		if rec.Code != http.StatusSeeOther {
			t.Fatalf("%s: status = %d body = %s", level, rec.Code, rec.Body.String())
		}
	}
	if lv.Level() != slog.LevelError {
		t.Errorf("log level = %v", lv.Level())
	}
	if retention.Load() != 14 {
		t.Errorf("retention = %d", retention.Load())
	}
	if e.h.config.Auth.DKIM != models.AuthModeEnforce || e.h.config.Retry.MaxRetries != 3 || !e.h.config.Retry.RetryOnTimeout {
		t.Errorf("config not applied: %+v", e.h.config)
	}

	invalid := map[string][2]string{
		"bad log level":       {"log_level", "verbose"},
		"bad auth mode":       {"auth_spf", "strict"},
		"bad max retries":     {"retry_max_retries", "-1"},
		"bad initial wait":    {"retry_initial_wait_seconds", "0"},
		"bad max wait":        {"retry_max_wait_seconds", "x"},
		"initial > max":       {"retry_initial_wait_seconds", "60"},
		"bad timeout":         {"retry_timeout_seconds", "-5"},
		"bad retention days":  {"retention_days", "-1"},
		"non-numeric retries": {"retry_max_retries", "many"},
	}
	for name, kv := range invalid {
		t.Run(name, func(t *testing.T) {
			form := validSettings()
			form.Set(kv[0], kv[1])
			rec := e.do(t, "POST", "/settings", form, false)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("status = %d", rec.Code)
			}
		})
	}

	req := httptest.NewRequest("POST", "/settings", strings.NewReader("%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed form status = %d", rec.Code)
	}
}

func validRuleForm(name string) url.Values {
	return url.Values{
		"name":                {name},
		"webhook_url":         {"https://example.com/hook"},
		"webhook_method":      {"PUT"},
		"webhook_headers":     {"Authorization: Bearer x\n\nX-Extra: y"},
		"payload_template":    {`{"subject": "{{.subject}}"}`},
		"wh_timeout":          {"5"},
		"wh_max_retries":      {"2"},
		"wh_initial_wait":     {"1"},
		"wh_max_wait":         {"10"},
		"wh_retry_on_timeout": {"yes"},
		"match_mode":          {"any"},
		"cond_field":          {"from", "header", ""},
		"cond_pattern":        {"*@example.com", "urgent", ""},
		"cond_header":         {"", "X-Priority", ""},
	}
}

func TestRulesCRUD(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	rec := e.do(t, "POST", "/rules", validRuleForm("alerts"), false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d body = %s", rec.Code, rec.Body.String())
	}
	list, _ := e.store.ListRules(ctx)
	if len(list) != 1 {
		t.Fatalf("rules = %d", len(list))
	}
	rule := list[0]
	if rule.Webhook.Method != "PUT" || rule.Webhook.Headers["X-Extra"] != "y" || rule.Match.Mode != "any" ||
		len(rule.Match.Conditions) != 2 || rule.Match.Conditions[1].Header != "X-Priority" ||
		*rule.Webhook.MaxRetries != 2 || !*rule.Webhook.RetryOnTimeout {
		t.Errorf("rule not parsed: %+v", rule)
	}

	rec = e.do(t, "GET", "/rules", nil, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "alerts") {
		t.Errorf("list status = %d", rec.Code)
	}

	rec = e.do(t, "POST", "/rules", validRuleForm("alerts"), false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate create status = %d", rec.Code)
	}

	rec = e.do(t, "GET", "/rules/"+rule.ID+"/edit", nil, false)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("edit form status = %d", rec.Code)
	}

	form := validRuleForm("alerts-renamed")
	form.Set("match_mode", "all")
	rec = e.do(t, "POST", "/rules/"+rule.ID, form, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("update status = %d", rec.Code)
	}
	got, _ := e.store.GetRule(ctx, rule.ID)
	if got.Name != "alerts-renamed" || got.Match.Mode != "all" {
		t.Errorf("rule not updated: %+v", got)
	}

	e.do(t, "POST", "/rules", validRuleForm("other"), false)
	rec = e.do(t, "POST", "/rules/"+rule.ID, validRuleForm("other"), false)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("duplicate update status = %d", rec.Code)
	}

	bad := validRuleForm("x")
	bad.Del("webhook_url")
	if rec := e.do(t, "POST", "/rules/"+rule.ID, bad, false); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid update status = %d", rec.Code)
	}
	if rec := e.do(t, "POST", "/rules/missing", validRuleForm("x"), false); rec.Code != http.StatusNotFound {
		t.Errorf("missing update status = %d", rec.Code)
	}

	rec = e.do(t, "POST", "/rules/"+rule.ID+"/toggle", nil, false)
	if rec.Code != http.StatusSeeOther {
		t.Errorf("toggle status = %d", rec.Code)
	}
	got, _ = e.store.GetRule(ctx, rule.ID)
	if got.Enabled {
		t.Errorf("rule still enabled after toggle")
	}
	if rec := e.do(t, "POST", "/rules/missing/toggle", nil, false); rec.Code != http.StatusNotFound {
		t.Errorf("missing toggle status = %d", rec.Code)
	}

	rec = e.do(t, "DELETE", "/rules/"+rule.ID, nil, true)
	if rec.Code != http.StatusOK || rec.Header().Get("HX-Redirect") != "/rules" {
		t.Errorf("delete status = %d", rec.Code)
	}
	if _, err := e.store.GetRule(ctx, rule.ID); err == nil {
		t.Errorf("rule still exists")
	}
}

func TestParseRuleFormErrors(t *testing.T) {
	cases := map[string][2]string{
		"missing name":       {"name", ""},
		"missing url":        {"webhook_url", ""},
		"bad template":       {"payload_template", "{{ .Nope"},
		"bad header":         {"webhook_headers", "no-colon-here"},
		"bad timeout":        {"wh_timeout", "-1"},
		"bad max retries":    {"wh_max_retries", "x"},
		"bad initial wait":   {"wh_initial_wait", "0"},
		"bad max wait":       {"wh_max_wait", "0"},
		"initial > max wait": {"wh_initial_wait", "60"},
		"header w/o name":    {"cond_header", ""},
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			form := validRuleForm("r")
			if kv[0] == "cond_header" {
				form["cond_header"] = []string{"", ""}
			} else {
				form.Set(kv[0], kv[1])
			}
			req := httptest.NewRequest("POST", "/rules", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.ParseForm()
			if _, msg := parseRuleForm(req); msg == "" {
				t.Errorf("expected an error")
			}
		})
	}

	form := url.Values{"name": {"r"}, "webhook_url": {"https://example.com"}}
	req := httptest.NewRequest("POST", "/rules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ParseForm()
	rule, msg := parseRuleForm(req)
	if msg != "" || rule.Webhook.Method != "POST" || rule.Match.Mode != "all" || rule.Webhook.Headers != nil {
		t.Errorf("defaults not applied: %q %+v", msg, rule)
	}
}

func TestTestWebhook(t *testing.T) {
	e := newTestEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("pong"))
	}))
	defer srv.Close()

	decode := func(rec *httptest.ResponseRecorder) map[string]any {
		var m map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	m := decode(e.do(t, "POST", "/rules/test-webhook", url.Values{"webhook_url": {srv.URL}, "webhook_method": {"POST"}}, true))
	if m["success"] != true || m["body"] != "pong" || m["status_code"] != float64(200) {
		t.Errorf("result = %v", m)
	}

	m = decode(e.do(t, "POST", "/rules/test-webhook", url.Values{"webhook_url": {""}}, true))
	if m["success"] != false || m["error"] != "URL is required" {
		t.Errorf("empty url result = %v", m)
	}

	m = decode(e.do(t, "POST", "/rules/test-webhook", url.Values{"webhook_url": {"http://127.0.0.1:1/"}}, true))
	if m["success"] != false {
		t.Errorf("unreachable result = %v", m)
	}
}

func TestStaticAssetsCached(t *testing.T) {
	e := newTestEnv(t)
	entries, err := staticFS.ReadDir("static")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no static files: %v", err)
	}
	rec := e.do(t, "GET", "/static/"+entries[0].Name(), nil, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=604800" {
		t.Errorf("cache control = %q", cc)
	}
}

func TestComputeStatus(t *testing.T) {
	ok := models.DeliveryResult{Status: "success"}
	fail := models.DeliveryResult{Status: "failed"}
	cases := []struct {
		in   []models.DeliveryResult
		want models.EmailStatus
	}{
		{nil, models.EmailStatusDropped},
		{[]models.DeliveryResult{ok, ok}, models.EmailStatusDelivered},
		{[]models.DeliveryResult{ok, fail}, models.EmailStatusPartialFailure},
		{[]models.DeliveryResult{fail}, models.EmailStatusFailed},
	}
	for _, c := range cases {
		if got := ComputeStatus(c.in); got != c.want {
			t.Errorf("ComputeStatus(%v) = %s, want %s", c.in, got, c.want)
		}
	}
}

func TestTemplateFuncs(t *testing.T) {
	e := newTestEnv(t)
	call := func(name string, args ...any) any {
		t.Helper()
		var b strings.Builder
		tmpl, err := e.h.templates.Clone()
		if err != nil {
			t.Fatal(err)
		}
		src := "{{" + name
		data := map[string]any{}
		for i, a := range args {
			key := string(rune('a' + i))
			data[key] = a
			src += " ." + key
		}
		src += "}}"
		if _, err := tmpl.New("t").Parse(src); err != nil {
			t.Fatal(err)
		}
		if err := tmpl.ExecuteTemplate(&b, "t", data); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return b.String()
	}

	now := time.Now()
	ago := map[time.Duration][2]string{
		10 * time.Second: {"just now", "now"},
		1 * time.Minute:  {"1 minute ago", "1m"},
		5 * time.Minute:  {"5 minutes ago", "5m"},
		1 * time.Hour:    {"1 hour ago", "1h"},
		3 * time.Hour:    {"3 hours ago", "3h"},
		24 * time.Hour:   {"1 day ago", "1d"},
		72 * time.Hour:   {"3 days ago", "3d"},
	}
	for d, want := range ago {
		ts := now.Add(-d - time.Second)
		if d == 10*time.Second {
			ts = now.Add(-d)
		}
		if got := call("timeAgo", ts); got != want[0] {
			t.Errorf("timeAgo(%v) = %q, want %q", d, got, want[0])
		}
		if got := call("timeAgoShort", ts); got != want[1] {
			t.Errorf("timeAgoShort(%v) = %q, want %q", d, got, want[1])
		}
	}

	for status, want := range map[models.EmailStatus]string{
		models.EmailStatusDelivered:      "green",
		models.EmailStatusDropped:        "amber",
		models.EmailStatusFailed:         "red",
		models.EmailStatusPartialFailure: "orange",
		models.EmailStatusRejected:       "red",
		models.EmailStatusReceived:       "blue",
		"other":                          "gray",
	} {
		if got := call("statusColor", status); got != want {
			t.Errorf("statusColor(%s) = %q", status, got)
		}
	}
	for status, want := range map[string]string{
		"success": "green", "failed": "red", "rejected": "orange", "error": "red", "cancelled": "gray", "": "gray",
	} {
		if got := call("deliveryStatusColor", status); got != want {
			t.Errorf("deliveryStatusColor(%s) = %q", status, got)
		}
	}

	checks := []struct {
		name string
		args []any
		want string
	}{
		{"statusLabel", []any{models.EmailStatusPartialFailure}, "Partial"},
		{"statusLabel", []any{models.EmailStatusDelivered}, "Delivered"},
		{"statusLabel", []any{models.EmailStatus("")}, ""},
		{"deliveryStatusLabel", []any{"success"}, "Success"},
		{"deliveryStatusLabel", []any{""}, ""},
		{"formatBytes", []any{int64(512)}, "512 B"},
		{"formatBytes", []any{int64(2048)}, "2.0 KB"},
		{"formatBytes", []any{int64(3 << 20)}, "3.0 MB"},
		{"formatBytes", []any{int64(5 << 30)}, "5.0 GB"},
		{"formatDuration", []any{90 * time.Second}, "2m"},
		{"formatDuration", []any{30 * time.Second}, "30s"},
		{"durationSeconds", []any{time.Minute}, "60"},
		{"derefBool", []any{(*bool)(nil)}, "false"},
		{"derefInt", []any{(*int)(nil)}, "0"},
		{"joinStrings", []any{[]string{"a", "b"}}, "a, b"},
		{"headersToText", []any{map[string]string{}}, ""},
		{"headersToText", []any{map[string]string{"A": "1"}}, "A: 1"},
		{"attachmentIndex", []any{3}, "3"},
		{"sub", []any{5, 2}, "3"},
		{"add", []any{5, 2}, "7"},
		{"pageNumbers", []any{5, 10}, "[3 4 5 6 7]"},
		{"conditionField", []any{models.MatchCondition{Field: "header", Header: "X-Foo"}}, "X-Foo"},
		{"conditionField", []any{models.MatchCondition{Field: "from"}}, "From"},
		{"conditionField", []any{models.MatchCondition{Field: "custom"}}, "custom"},
	}
	for _, c := range checks {
		if got := call(c.name, c.args...); got != c.want {
			t.Errorf("%s%v = %q, want %q", c.name, c.args, got, c.want)
		}
	}
	tr, n := true, 7
	if got := call("derefBool", &tr); got != "true" {
		t.Errorf("derefBool(&true) = %q", got)
	}
	if got := call("derefInt", &n); got != "7" {
		t.Errorf("derefInt(&7) = %q", got)
	}
	if got := call("formatTime", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)); got != "Jan 02, 2026 03:04:05 UTC" {
		t.Errorf("formatTime = %q", got)
	}
}

func TestRuleSigningSecretLifecycle(t *testing.T) {
	e := newTestEnv(t)
	form := url.Values{"name": {"signed"}, "webhook_url": {"https://example.com"}, "signing_secret": {"private-secret"}}
	if rec := e.do(t, "POST", "/rules", form, false); rec.Code != http.StatusSeeOther {
		t.Fatal(rec.Code)
	}
	records, err := e.store.ListRules(context.Background())
	if err != nil || len(records) != 1 {
		t.Fatalf("rules: %v %v", records, err)
	}
	id := records[0].ID
	if records[0].Webhook.SigningSecret != "private-secret" {
		t.Fatal("secret not saved")
	}
	page := e.do(t, "GET", "/rules", nil, false).Body.String()
	if strings.Contains(page, "private-secret") || !strings.Contains(page, `data-rule-signed="true"`) {
		t.Fatal("secret leaked or signed status missing")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Mailrelay-Signature") == "" {
			t.Error("test did not use stored secret")
		}
	}))
	defer srv.Close()
	e.do(t, "POST", "/rules/test-webhook", url.Values{"webhook_url": {srv.URL}, "rule_id": {id}}, true)
	for _, step := range []struct{ value, clear, want string }{{"", "", "private-secret"}, {"replacement", "", "replacement"}, {"", "on", ""}} {
		form.Set("signing_secret", step.value)
		form.Set("clear_signing_secret", step.clear)
		if rec := e.do(t, "POST", "/rules/"+id, form, false); rec.Code != http.StatusSeeOther {
			t.Fatal(rec.Code)
		}
		record, err := e.store.GetRule(context.Background(), id)
		if err != nil || record.Webhook.SigningSecret != step.want {
			t.Fatalf("secret = %+v, err %v", record, err)
		}
	}
}

func TestTestWebhookSigning(t *testing.T) {
	e := newTestEnv(t)
	rule := &models.RuleRecord{Name: "signed test", Webhook: models.WebhookConfig{SigningSecret: "saved-secret"}}
	if err := e.store.SaveRule(context.Background(), rule); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, ruleID, entered, clear, wantSecret string
	}{
		{name: "unsigned new rule"},
		{name: "new secret", entered: "new-secret", wantSecret: "new-secret"},
		{name: "preserve saved secret", ruleID: rule.ID, wantSecret: "saved-secret"},
		{name: "replace saved secret", ruleID: rule.ID, entered: "replacement", wantSecret: "replacement"},
		{name: "clear saved secret", ruleID: rule.ID, clear: "on"},
		{name: "clear overrides entered secret", ruleID: rule.ID, entered: "replacement", clear: "on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if string(body) != "{}" {
					t.Errorf("test body = %q, want {}", body)
				}
				timestamp := r.Header.Get("X-Mailrelay-Timestamp")
				signature := r.Header.Get("X-Mailrelay-Signature")
				deliveryID := r.Header.Get("X-Mailrelay-Delivery")
				if tc.wantSecret == "" {
					if timestamp != "" || signature != "" || deliveryID != "" {
						t.Error("unsigned Test request has signing headers")
					}
					return
				}
				if timestamp == "" || deliveryID == "" {
					t.Error("missing timestamp or delivery ID")
				}
				mac := hmac.New(sha256.New, []byte(tc.wantSecret))
				mac.Write([]byte(timestamp + "."))
				mac.Write(body)
				if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); signature != want {
					t.Errorf("signature = %q, want %q", signature, want)
				}
			}))
			defer srv.Close()
			rec := e.do(t, "POST", "/rules/test-webhook", url.Values{
				"webhook_url": {srv.URL}, "rule_id": {tc.ruleID},
				"signing_secret": {tc.entered}, "clear_signing_secret": {tc.clear},
			}, true)
			var result struct {
				Success bool `json:"success"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusOK || !result.Success || calls.Load() != 1 {
				t.Fatalf("status = %d, body = %s, requests = %d", rec.Code, rec.Body, calls.Load())
			}
			stored, err := e.store.GetRule(context.Background(), rule.ID)
			if err != nil || stored.Webhook.SigningSecret != "saved-secret" {
				t.Fatalf("Test modified saved secret: %+v, %v", stored, err)
			}
		})
	}
}

func TestTestWebhookMissingRule(t *testing.T) {
	e := newTestEnv(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()
	rec := e.do(t, "POST", "/rules/test-webhook", url.Values{
		"webhook_url": {srv.URL}, "rule_id": {"missing-rule"}, "signing_secret": {"do-not-send"},
	}, true)
	if rec.Code != http.StatusNotFound || strings.TrimSpace(rec.Body.String()) != "Rule not found" {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if calls.Load() != 0 {
		t.Fatal("sent webhook despite missing rule")
	}
}

func TestReplayQueueFailure(t *testing.T) {
	e := newTestEnv(t)
	id := e.saveEmail(t, "Queue unavailable", models.EmailStatusFailed)
	e.saveRule(t, "queue failure", "http://127.0.0.1:1")
	// Fail after the delivery row is inserted to exercise transactional rollback.
	db, err := sql.Open("sqlite", e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_enqueue BEFORE INSERT ON delivery_queue BEGIN SELECT RAISE(ABORT,'queue unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	response := e.do(t, http.MethodPost, "/emails/"+id+"/replay", nil, true)
	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "Failed to enqueue replay") {
		t.Fatalf("response=%d %s", response.Code, response.Body.String())
	}
	rows, err := e.store.GetDeliveriesForEmail(context.Background(), id)
	if err != nil || len(rows) != 0 {
		t.Fatalf("partial replay committed: %+v,%v", rows, err)
	}
	email, err := e.store.GetEmail(context.Background(), id)
	if err != nil || email.Status != models.EmailStatusFailed {
		t.Fatalf("failed replay changed email: %+v,%v", email, err)
	}
}
