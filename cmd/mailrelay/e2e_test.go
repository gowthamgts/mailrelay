package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
	smtpserver "github.com/gowthamgts/mailrelay/internal/smtp"
	"github.com/gowthamgts/mailrelay/internal/storage"
	"github.com/gowthamgts/mailrelay/internal/webhook"
)

// testHarness wires up the full pipeline: SMTP server → rules engine → webhook dispatcher.
type testHarness struct {
	engine     *rules.Engine
	dispatcher *webhook.Dispatcher
	smtpAddr   string
	server     *smtpserver.Server
	cancel     context.CancelFunc
}

func newTestHarness(t *testing.T, smtpCfg models.SMTPConfig) *testHarness {
	t.Helper()

	engine := rules.NewEngine()
	dispatcher := webhook.NewDispatcher(models.RetryConfig{
		MaxRetries:     2,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        5 * time.Second,
		RetryOnTimeout: true,
	}, "dev")

	ctx, cancel := context.WithCancel(context.Background())
	handler := buildHandler(engine, dispatcher, nil, nil, "")
	srv := smtpserver.NewServer(ctx, smtpCfg, models.AuthConfig{
		SPF:   models.AuthModeOff,
		DKIM:  models.AuthModeOff,
		DMARC: models.AuthModeOff,
	}, handler)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	// Wait until the port is reachable.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", smtpCfg.Addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	})

	return &testHarness{
		engine:     engine,
		dispatcher: dispatcher,
		smtpAddr:   smtpCfg.Addr,
		server:     srv,
		cancel:     cancel,
	}
}

// freePort returns an available TCP port on localhost.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// sendEmail sends a minimal RFC 5322 message via SMTP.
func sendEmail(t *testing.T, smtpAddr, from string, to []string, raw string) error {
	t.Helper()
	return smtp.SendMail(smtpAddr, nil, from, to, []byte(raw))
}

// waitForCalls blocks until count reaches want or timeout elapses.
func waitForCalls(t *testing.T, count *atomic.Int32, want int32, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("timed out waiting for %d calls; got %d", want, count.Load())
}

// webhookServer starts a test HTTP server and returns its URL, a call counter, and a
// slice of captured request bodies (protected by a mutex).
func webhookServer(t *testing.T) (url string, calls *atomic.Int32, bodies func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var captured [][]byte
	var cnt atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		captured = append(captured, b)
		mu.Unlock()
		cnt.Add(1) // increment AFTER body is stored so waitForCalls guarantees body visibility
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &cnt, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		out := make([][]byte, len(captured))
		copy(out, captured)
		return out
	}
}

// smtpCfg returns a minimal SMTP config bound to a free port.
func smtpCfg(t *testing.T) models.SMTPConfig {
	return models.SMTPConfig{
		Addr:            freePort(t),
		Domain:          "test.local",
		MaxMessageBytes: 1 << 20,
		MaxRecipients:   10,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
	}
}

// rawMessage builds a minimal RFC 5322 message string.
func rawMessage(from, to, subject, body string) string {
	return fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s", from, to, subject, body)
}

// --- E2E Tests ---

// TestE2E_BasicDelivery verifies the happy path:
// send email → match rule by from_domain → webhook fires exactly once.
func TestE2E_BasicDelivery(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "from-example",
			Match: models.MatcherConfig{Conditions: []models.MatchCondition{
				{Field: "from_domain", Pattern: "example.com"},
			}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "alice@example.com", []string{"bob@dest.com"},
		rawMessage("alice@example.com", "bob@dest.com", "Hello", "Basic e2e test")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 webhook call, got %d", calls.Load())
	}
}

// TestE2E_NoRuleMatch verifies that an email matching no rules fires no webhooks.
func TestE2E_NoRuleMatch(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "only-alerts",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "to_domain", Pattern: "alerts.example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@other.com", []string{"user@dest.com"},
		rawMessage("sender@other.com", "user@dest.com", "Not matching", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	// Give the pipeline time to process, then assert no calls were made.
	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("expected 0 webhook calls for no-match, got %d", calls.Load())
	}
}

// TestE2E_MultipleRulesMatch verifies that multiple matching rules each fire their own webhook.
func TestE2E_MultipleRulesMatch(t *testing.T) {
	hookURL1, calls1, _ := webhookServer(t)
	hookURL2, calls2, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "rule-domain",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL1, Method: "POST"},
		},
		{
			Name:    "rule-subject",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "subject", Pattern: "ALERT*"}}},
			Webhook: models.WebhookConfig{URL: hookURL2, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "monitor@example.com", []string{"ops@company.com"},
		rawMessage("monitor@example.com", "ops@company.com", "ALERT: CPU high", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls1, 1, 3*time.Second)
	waitForCalls(t, calls2, 1, 3*time.Second)
	if calls1.Load() != 1 {
		t.Errorf("rule-domain: expected 1 call, got %d", calls1.Load())
	}
	if calls2.Load() != 1 {
		t.Errorf("rule-subject: expected 1 call, got %d", calls2.Load())
	}
}

// TestE2E_GlobPatterns validates glob matching on all matcher fields.
func TestE2E_GlobPatterns(t *testing.T) {
	tests := []struct {
		name    string
		matcher models.MatcherConfig
		from    string
		to      string
		subject string
		want    bool
	}{
		{
			name:    "wildcard from_email",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "mail_from", Pattern: "*@alerts.io"}}},
			from:    "noreply@alerts.io",
			to:      "admin@corp.com",
			subject: "test",
			want:    true,
		},
		{
			name:    "wildcard to_email",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "rcpt_to", Pattern: "ops*@corp.com"}}},
			from:    "sender@foo.com",
			to:      "ops-team@corp.com",
			subject: "test",
			want:    true,
		},
		{
			name:    "glob subject prefix",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "subject", Pattern: "CRITICAL:*"}}},
			from:    "sender@foo.com",
			to:      "r@bar.com",
			subject: "CRITICAL: disk full",
			want:    true,
		},
		{
			name:    "glob to_domain wildcard",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "to_domain", Pattern: "*.company.com"}}},
			from:    "sender@foo.com",
			to:      "r@sub.company.com",
			subject: "test",
			want:    true,
		},
		{
			name:    "from_email no match",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "mail_from", Pattern: "*@alerts.io"}}},
			from:    "sender@other.com",
			to:      "r@bar.com",
			subject: "test",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hookURL, calls, _ := webhookServer(t)
			h := newTestHarness(t, smtpCfg(t))
			h.engine.SetRules([]models.Rule{
				{Name: "rule", Match: tt.matcher, Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"}},
			})

			if err := sendEmail(t, h.smtpAddr, tt.from, []string{tt.to},
				rawMessage(tt.from, tt.to, tt.subject, "body")); err != nil {
				t.Fatalf("sendEmail: %v", err)
			}

			if tt.want {
				waitForCalls(t, calls, 1, 3*time.Second)
				if calls.Load() != 1 {
					t.Errorf("expected 1 call, got %d", calls.Load())
				}
			} else {
				time.Sleep(300 * time.Millisecond)
				if calls.Load() != 0 {
					t.Errorf("expected 0 calls for no-match, got %d", calls.Load())
				}
			}
		})
	}
}

// TestE2E_RegexPatterns validates regex matching on matcher fields.
func TestE2E_RegexPatterns(t *testing.T) {
	tests := []struct {
		name    string
		matcher models.MatcherConfig
		from    string
		to      string
		subject string
		want    bool
	}{
		{
			name:    "regex subject match",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "subject", Pattern: `/^\[URGENT\]/`}}},
			from:    "s@foo.com",
			to:      "r@bar.com",
			subject: "[URGENT] server down",
			want:    true,
		},
		{
			name:    "regex from_email match",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "mail_from", Pattern: `/^(monitor|alert)@/`}}},
			from:    "monitor@infra.com",
			to:      "r@bar.com",
			subject: "test",
			want:    true,
		},
		{
			name:    "regex subject no match",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "subject", Pattern: `/^\[URGENT\]/`}}},
			from:    "s@foo.com",
			to:      "r@bar.com",
			subject: "ordinary subject",
			want:    false,
		},
		{
			name:    "regex to_email match",
			matcher: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "rcpt_to", Pattern: `/^(ops|sre)@company\.com$/`}}},
			from:    "s@foo.com",
			to:      "sre@company.com",
			subject: "test",
			want:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hookURL, calls, _ := webhookServer(t)
			h := newTestHarness(t, smtpCfg(t))
			h.engine.SetRules([]models.Rule{
				{Name: "rule", Match: tt.matcher, Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"}},
			})

			if err := sendEmail(t, h.smtpAddr, tt.from, []string{tt.to},
				rawMessage(tt.from, tt.to, tt.subject, "body")); err != nil {
				t.Fatalf("sendEmail: %v", err)
			}

			if tt.want {
				waitForCalls(t, calls, 1, 3*time.Second)
				if calls.Load() != 1 {
					t.Errorf("expected 1 call, got %d", calls.Load())
				}
			} else {
				time.Sleep(300 * time.Millisecond)
				if calls.Load() != 0 {
					t.Errorf("expected 0 calls for no-match, got %d", calls.Load())
				}
			}
		})
	}
}

// TestE2E_ANDLogic verifies that all matcher fields must match for a rule to fire.
func TestE2E_ANDLogic(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name: "and-rule",
			Match: models.MatcherConfig{Conditions: []models.MatchCondition{
				{Field: "from_domain", Pattern: "example.com"},
				{Field: "subject", Pattern: "ALERT*"},
				{Field: "to_domain", Pattern: "ops.io"},
			}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// Should match — all three conditions satisfied.
	if err := sendEmail(t, h.smtpAddr, "mon@example.com", []string{"oncall@ops.io"},
		rawMessage("mon@example.com", "oncall@ops.io", "ALERT: high load", "body")); err != nil {
		t.Fatalf("sendEmail (match): %v", err)
	}
	waitForCalls(t, calls, 1, 3*time.Second)

	// Should NOT match — wrong domain.
	if err := sendEmail(t, h.smtpAddr, "mon@other.com", []string{"oncall@ops.io"},
		rawMessage("mon@other.com", "oncall@ops.io", "ALERT: high load", "body")); err != nil {
		t.Fatalf("sendEmail (no-match): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// Should NOT match — subject doesn't start with ALERT.
	if err := sendEmail(t, h.smtpAddr, "mon@example.com", []string{"oncall@ops.io"},
		rawMessage("mon@example.com", "oncall@ops.io", "Weekly report", "body")); err != nil {
		t.Fatalf("sendEmail (no-match subject): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 call (AND logic), got %d", calls.Load())
	}
}

// TestE2E_ORLogicMultipleRecipients verifies OR logic across envelope To addresses.
func TestE2E_ORLogicMultipleRecipients(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "ops-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "rcpt_to", Pattern: "ops@company.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// Send to two recipients, one of which matches.
	if err := sendEmail(t, h.smtpAddr, "sender@foo.com",
		[]string{"dev@company.com", "ops@company.com"},
		rawMessage("sender@foo.com", "dev@company.com, ops@company.com", "Hello", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (OR across To addresses), got %d", calls.Load())
	}
}

// TestE2E_WebhookPayloadContents verifies the webhook receives correct JSON with email fields.
func TestE2E_WebhookPayloadContents(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "capture",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "alice@example.com", []string{"bob@dest.com"},
		rawMessage("alice@example.com", "bob@dest.com", "Payload check", "Hello from Alice")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no webhook bodies captured")
	}

	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal webhook body: %v\nbody: %s", err, bs[0])
	}

	if parsed.EnvelopeFrom != "alice@example.com" {
		t.Errorf("EnvelopeFrom = %q, want alice@example.com", parsed.EnvelopeFrom)
	}
	if parsed.Subject != "Payload check" {
		t.Errorf("Subject = %q, want 'Payload check'", parsed.Subject)
	}
	if !strings.Contains(parsed.TextBody, "Hello from Alice") {
		t.Errorf("TextBody = %q, want to contain 'Hello from Alice'", parsed.TextBody)
	}
	if len(parsed.EnvelopeTo) == 0 || parsed.EnvelopeTo[0] != "bob@dest.com" {
		t.Errorf("EnvelopeTo = %v, want [bob@dest.com]", parsed.EnvelopeTo)
	}
}

// TestE2E_CustomPayloadTemplate verifies Go template-based payload rendering.
func TestE2E_CustomPayloadTemplate(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:  "template-rule",
			Match: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{
				URL:             hookURL,
				Method:          "POST",
				PayloadTemplate: `{"alert_from":{{.From}},"alert_subject":{{.Subject}}}`,
			},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "monitor@example.com", []string{"ops@company.com"},
		rawMessage("monitor@example.com", "ops@company.com", "Server down", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no webhook bodies captured")
	}

	var result map[string]string
	if err := json.Unmarshal(bs[0], &result); err != nil {
		t.Fatalf("unmarshal custom template body: %v\nbody: %s", err, bs[0])
	}

	if result["alert_subject"] != "Server down" {
		t.Errorf("alert_subject = %q, want 'Server down'", result["alert_subject"])
	}
}

// TestE2E_CustomWebhookHeaders verifies that custom headers are forwarded to the webhook.
func TestE2E_CustomWebhookHeaders(t *testing.T) {
	var capturedToken string
	var mu sync.Mutex
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		capturedToken = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:  "auth-rule",
			Match: models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{
				URL:    srv.URL,
				Method: "POST",
				Headers: map[string]string{
					"Authorization": "Bearer secret-token",
				},
			},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "alice@example.com", []string{"bob@dest.com"},
		rawMessage("alice@example.com", "bob@dest.com", "Auth header test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, &calls, 1, 3*time.Second)

	mu.Lock()
	token := capturedToken
	mu.Unlock()

	if token != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want 'Bearer secret-token'", token)
	}
}

// TestE2E_WebhookRetryOn5xx verifies that failed (5xx) webhooks are retried.
func TestE2E_WebhookRetryOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "retry-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Retry test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	// Wait for 3 calls: 1 initial + 2 retries.
	waitForCalls(t, &calls, 3, 5*time.Second)
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", calls.Load())
	}
}

// TestE2E_WebhookNoRetryOn4xx verifies that 4xx responses are not retried.
func TestE2E_WebhookNoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "no-retry-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "No retry test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	// Give the pipeline enough time to potentially retry (it shouldn't).
	time.Sleep(500 * time.Millisecond)
	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 call (no retry on 4xx), got %d", calls.Load())
	}
}

// TestE2E_AllowedRecipientsFilter verifies that emails to disallowed recipients are rejected at SMTP.
func TestE2E_AllowedRecipientsFilter(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	cfg := smtpCfg(t)
	cfg.AllowedRecipients = []string{"*@alerts.company.com"}
	h := newTestHarness(t, cfg)
	h.engine.SetRules([]models.Rule{
		{
			Name:    "allow-all",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// Allowed recipient — should succeed and fire webhook.
	if err := sendEmail(t, h.smtpAddr, "sender@foo.com", []string{"oncall@alerts.company.com"},
		rawMessage("sender@foo.com", "oncall@alerts.company.com", "Allowed", "body")); err != nil {
		t.Fatalf("sendEmail to allowed recipient: %v", err)
	}
	waitForCalls(t, calls, 1, 3*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 call for allowed recipient, got %d", calls.Load())
	}

	// Disallowed recipient — should be rejected at SMTP level.
	err := sendEmail(t, h.smtpAddr, "sender@foo.com", []string{"user@other.com"},
		rawMessage("sender@foo.com", "user@other.com", "Disallowed", "body"))
	if err == nil {
		t.Error("expected SMTP error for disallowed recipient, got nil")
	}

	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 1 {
		t.Errorf("expected still 1 call after disallowed email, got %d", calls.Load())
	}
}

// TestE2E_HTMLEmail verifies that HTML body is parsed and forwarded to the webhook.
func TestE2E_HTMLEmail(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "html-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	htmlMsg := "From: sender@example.com\r\n" +
		"To: r@dest.com\r\n" +
		"Subject: HTML email\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<html><body><h1>Hello World</h1></body></html>"

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"}, htmlMsg); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}

	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(parsed.HTMLBody, "Hello World") {
		t.Errorf("HTMLBody = %q, expected to contain 'Hello World'", parsed.HTMLBody)
	}
}

// TestE2E_MultipartAlternativeEmail verifies multipart/alternative parsing (prefer HTML).
func TestE2E_MultipartAlternativeEmail(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "multipart-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	multipartMsg := "From: sender@example.com\r\n" +
		"To: r@dest.com\r\n" +
		"Subject: Multipart test\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"boundary42\"\r\n" +
		"\r\n" +
		"--boundary42\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"Plain text body\r\n" +
		"--boundary42\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n" +
		"\r\n" +
		"<p>HTML body</p>\r\n" +
		"--boundary42--\r\n"

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"}, multipartMsg); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}

	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(parsed.TextBody, "Plain text body") {
		t.Errorf("TextBody = %q, want to contain 'Plain text body'", parsed.TextBody)
	}
	if !strings.Contains(parsed.HTMLBody, "HTML body") {
		t.Errorf("HTMLBody = %q, want to contain 'HTML body'", parsed.HTMLBody)
	}
}

// TestE2E_RulesHotReload verifies that updating rules mid-run takes effect immediately.
func TestE2E_RulesHotReload(t *testing.T) {
	hookURL1, calls1, _ := webhookServer(t)
	hookURL2, calls2, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))

	// Start with rule1 only.
	h.engine.SetRules([]models.Rule{
		{
			Name:    "rule1",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL1, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "a@example.com", []string{"r@dest.com"},
		rawMessage("a@example.com", "r@dest.com", "Before reload", "body")); err != nil {
		t.Fatalf("sendEmail (before reload): %v", err)
	}
	waitForCalls(t, calls1, 1, 3*time.Second)

	// Hot-reload: replace with rule2.
	h.engine.SetRules([]models.Rule{
		{
			Name:    "rule2",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL2, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "b@example.com", []string{"r@dest.com"},
		rawMessage("b@example.com", "r@dest.com", "After reload", "body")); err != nil {
		t.Fatalf("sendEmail (after reload): %v", err)
	}
	waitForCalls(t, calls2, 1, 3*time.Second)

	if calls1.Load() != 1 {
		t.Errorf("rule1 calls = %d, want 1", calls1.Load())
	}
	if calls2.Load() != 1 {
		t.Errorf("rule2 calls = %d, want 1", calls2.Load())
	}
}

// TestE2E_ConcurrentEmails verifies that multiple emails sent concurrently
// each trigger exactly one webhook call.
func TestE2E_ConcurrentEmails(t *testing.T) {
	const n = 5
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "concurrent-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			subject := fmt.Sprintf("Concurrent #%d", i)
			if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
				rawMessage("sender@example.com", "r@dest.com", subject, "body")); err != nil {
				t.Errorf("sendEmail %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	waitForCalls(t, calls, int32(n), 5*time.Second)
	if calls.Load() != int32(n) {
		t.Errorf("expected %d webhook calls, got %d", n, calls.Load())
	}
}

// TestE2E_WildcardMatcherMatchesEverything verifies an empty-pattern rule fires for all emails.
func TestE2E_WildcardMatcherMatchesEverything(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "catch-all",
			Match:   models.MatcherConfig{}, // no matchers = matches everything
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	senders := []string{"a@foo.com", "b@bar.org", "c@baz.net"}
	for i, sender := range senders {
		if err := sendEmail(t, h.smtpAddr, sender, []string{"r@dest.com"},
			rawMessage(sender, "r@dest.com", fmt.Sprintf("Email %d", i), "body")); err != nil {
			t.Fatalf("sendEmail %d: %v", i, err)
		}
	}

	waitForCalls(t, calls, int32(len(senders)), 5*time.Second)
	if calls.Load() != int32(len(senders)) {
		t.Errorf("expected %d calls, got %d", len(senders), calls.Load())
	}
}

// TestE2E_ToDomainORLogic verifies that if any envelope To domain matches, the rule fires.
func TestE2E_ToDomainORLogic(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "ops-domain",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "rcpt_to_domain", Pattern: "ops.io"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// Send to two recipients with different domains; one matches.
	if err := sendEmail(t, h.smtpAddr, "sender@foo.com",
		[]string{"team@dev.io", "oncall@ops.io"},
		rawMessage("sender@foo.com", "team@dev.io", "multi-to", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (OR across To domains), got %d", calls.Load())
	}
}

// TestE2E_WebhookReceivesAllEmailFields ensures complex email fields are present in webhook payload.
func TestE2E_WebhookReceivesAllEmailFields(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "fields-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	msg := "From: \"Alice Smith\" <alice@example.com>\r\n" +
		"To: bob@dest.com\r\n" +
		"Cc: carol@cc.com\r\n" +
		"Subject: Fields test\r\n" +
		"\r\n" +
		"Body text here"

	if err := sendEmail(t, h.smtpAddr, "alice@example.com", []string{"bob@dest.com"}, msg); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}

	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, bs[0])
	}

	if !strings.Contains(parsed.From, "alice@example.com") {
		t.Errorf("From = %q, want to contain alice@example.com", parsed.From)
	}
	if parsed.Subject != "Fields test" {
		t.Errorf("Subject = %q, want 'Fields test'", parsed.Subject)
	}
	if !strings.Contains(parsed.TextBody, "Body text here") {
		t.Errorf("TextBody = %q, want to contain 'Body text here'", parsed.TextBody)
	}
}

// TestE2E_NoRulesConfigured verifies that with no rules, emails are silently dropped.
func TestE2E_NoRulesConfigured(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	// Intentionally no rules set.
	_ = hookURL

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Dropped", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	time.Sleep(300 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("expected 0 webhook calls with no rules, got %d", calls.Load())
	}
}

// --- Auth Tests ---

// newTestHarnessWithAuth creates a harness with specified auth configuration.
func newTestHarnessWithAuth(t *testing.T, cfg models.SMTPConfig, authCfg models.AuthConfig) *testHarness {
	t.Helper()
	engine := rules.NewEngine()
	dispatcher := webhook.NewDispatcher(models.RetryConfig{
		MaxRetries:     2,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        5 * time.Second,
		RetryOnTimeout: true,
	}, "dev")
	ctx, cancel := context.WithCancel(context.Background())
	handler := buildHandler(engine, dispatcher, nil, nil, "")
	srv := smtpserver.NewServer(ctx, cfg, authCfg, handler)

	go srv.ListenAndServe() //nolint:errcheck
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", cfg.Addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	})
	return &testHarness{engine: engine, dispatcher: dispatcher, smtpAddr: cfg.Addr, server: srv, cancel: cancel}
}

// newTestHarnessWithStore creates a harness wired to an in-memory SQLite store so
// the rejection handler path can be exercised.
func newTestHarnessWithStore(t *testing.T, cfg models.SMTPConfig, authCfg models.AuthConfig) (*testHarness, *storage.Store) {
	t.Helper()
	store, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	engine := rules.NewEngine()
	dispatcher := webhook.NewDispatcher(models.RetryConfig{
		MaxRetries:     1,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        5 * time.Second,
		RetryOnTimeout: true,
	}, "dev")
	ctx, cancel := context.WithCancel(context.Background())
	handler := buildHandler(engine, dispatcher, store, nil, "")
	srv := smtpserver.NewServer(ctx, cfg, authCfg, handler)
	srv.SetRejectionHandler(buildRejectionHandler(store, nil, ""))

	go srv.ListenAndServe() //nolint:errcheck
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", cfg.Addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutCancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	})
	return &testHarness{engine: engine, dispatcher: dispatcher, smtpAddr: cfg.Addr, server: srv, cancel: cancel}, store
}

// invalidDKIMMessage returns a raw email with a syntactically valid but
// unverifiable DKIM-Signature header. The 'd=' domain is .invalid so the
// public-key DNS lookup will fail immediately, causing the verifier to report
// DKIM=fail without any network latency.
func invalidDKIMMessage(from, to, subject string) string {
	return fmt.Sprintf(
		"From: %s\r\n"+
			"To: %s\r\n"+
			"Subject: %s\r\n"+
			"DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed;"+
			" d=dkim-test.invalid; s=key1; h=from:to:subject;"+
			" bh=47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=;"+
			" b=invalidsignatureAAAAAAAAAAAAAAAAAAAAAAAA\r\n"+
			"\r\n"+
			"Body with invalid DKIM signature",
		from, to, subject,
	)
}

// TestE2E_DKIMLog_InvalidSignature verifies that with DKIM in log mode an email
// carrying an unverifiable DKIM signature is still delivered, and the webhook
// payload reflects DKIM=fail in the auth result.
func TestE2E_DKIMLog_InvalidSignature(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarnessWithAuth(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeOff,
		DKIM:  models.AuthModeLog,
		DMARC: models.AuthModeOff,
	})
	h.engine.SetRules([]models.Rule{
		{
			Name:    "dkim-log-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		invalidDKIMMessage("sender@example.com", "r@dest.com", "DKIM log test")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 5*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 webhook call (DKIM log → deliver), got %d", calls.Load())
	}

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no webhook bodies captured")
	}
	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.AuthResult == nil {
		t.Fatal("expected non-nil AuthResult in payload")
	}
	if parsed.AuthResult.DKIM != models.AuthFail {
		t.Errorf("DKIM = %q, want fail", parsed.AuthResult.DKIM)
	}
}

// TestE2E_DKIMEnforce_InvalidSignature verifies that with DKIM in enforce mode
// an email with an unverifiable signature is rejected with an SMTP 550 error.
func TestE2E_DKIMEnforce_InvalidSignature(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarnessWithAuth(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeOff,
		DKIM:  models.AuthModeEnforce,
		DMARC: models.AuthModeOff,
	})
	h.engine.SetRules([]models.Rule{
		{
			Name:    "dkim-enforce-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		invalidDKIMMessage("sender@example.com", "r@dest.com", "DKIM enforce test"))
	if err == nil {
		t.Error("expected SMTP rejection error for DKIM enforce failure, got nil")
	} else if !strings.Contains(err.Error(), "550") {
		t.Errorf("expected 550 error, got: %v", err)
	}

	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 0 {
		t.Errorf("expected 0 webhook calls after DKIM enforcement rejection, got %d", calls.Load())
	}
}

// TestE2E_SPFLog_NoneNotRejected verifies that with SPF in log mode, an email from
// a domain with no SPF record (result=none) is still delivered. SPF "none" is not
// a failure; only "fail"/"softfail" counts as a failure.
func TestE2E_SPFLog_NoneNotRejected(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarnessWithAuth(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeLog,
		DKIM:  models.AuthModeOff,
		DMARC: models.AuthModeOff,
	})
	h.engine.SetRules([]models.Rule{
		{
			Name:    "spf-log-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// test.invalid has no SPF records → result will be SPF=none (not fail).
	if err := sendEmail(t, h.smtpAddr, "sender@test.invalid", []string{"r@dest.com"},
		rawMessage("sender@test.invalid", "r@dest.com", "SPF log test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 5*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (SPF=none should not reject), got %d", calls.Load())
	}

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}
	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.AuthResult == nil {
		t.Fatal("expected non-nil AuthResult")
	}
	// SPF=none means no record found, which is not a failure.
	if parsed.AuthResult.SPF == models.AuthFail {
		t.Errorf("SPF = %q, want none (no SPF record ≠ failure)", parsed.AuthResult.SPF)
	}
}

// TestE2E_RejectionHandlerWithStore verifies that when DKIM enforcement rejects
// an email, the rejection handler stores it with status=rejected in the DB.
func TestE2E_RejectionHandlerWithStore(t *testing.T) {
	h, store := newTestHarnessWithStore(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeOff,
		DKIM:  models.AuthModeEnforce,
		DMARC: models.AuthModeOff,
	})
	// No rules needed — rejection happens before the rule engine.
	_ = h

	err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		invalidDKIMMessage("sender@example.com", "r@dest.com", "Rejection storage test"))
	if err == nil {
		t.Fatal("expected SMTP rejection, got nil")
	}

	// Give the rejection handler goroutine time to write to the DB.
	deadline := time.Now().Add(3 * time.Second)
	var result *storage.EmailListResult
	for time.Now().Before(deadline) {
		r, err := store.ListEmails(context.Background(), storage.EmailFilter{Status: "rejected"})
		if err != nil {
			t.Fatalf("ListEmails: %v", err)
		}
		if r.Total > 0 {
			result = r
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if result == nil || result.Total == 0 {
		t.Fatal("expected rejected email to be stored in DB")
	}
	rec := result.Emails[0]
	if rec.Status != models.EmailStatusRejected {
		t.Errorf("Status = %q, want rejected", rec.Status)
	}
	if rec.RejectionReason == "" {
		t.Error("expected non-empty RejectionReason")
	}
	if rec.EnvelopeFrom != "sender@example.com" {
		t.Errorf("EnvelopeFrom = %q, want sender@example.com", rec.EnvelopeFrom)
	}
}

// --- Webhook Outcome Tests ---

// TestE2E_WebhookPermanentFailure verifies that when all retry attempts return
// 5xx, the dispatcher exhausts its retries and stops calling the webhook.
func TestE2E_WebhookPermanentFailure(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "permanent-fail-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Permanent failure", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	// MaxRetries=2 means 1 initial + 2 retries = 3 total calls, then stop.
	waitForCalls(t, &calls, 3, 5*time.Second)
	// Wait a bit more to confirm no extra calls after exhaustion.
	time.Sleep(200 * time.Millisecond)
	if calls.Load() != 3 {
		t.Errorf("expected exactly 3 calls (1 initial + 2 retries), got %d", calls.Load())
	}
}

// TestE2E_WebhookGETMethod verifies that the dispatcher honours the HTTP method
// specified in the webhook config.
func TestE2E_WebhookGETMethod(t *testing.T) {
	var capturedMethod string
	var mu sync.Mutex
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		capturedMethod = r.Method
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "get-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "example.com"}}},
			Webhook: models.WebhookConfig{URL: srv.URL, Method: "GET"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "GET method test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, &calls, 1, 3*time.Second)

	mu.Lock()
	method := capturedMethod
	mu.Unlock()

	if method != "GET" {
		t.Errorf("HTTP method = %q, want GET", method)
	}
}

// TestE2E_WebhookCustomContentType verifies that a custom Content-Type header
// in the webhook config overrides the default application/json.
func TestE2E_WebhookCustomContentType(t *testing.T) {
	var capturedCT string
	var mu sync.Mutex
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mu.Lock()
		capturedCT = r.Header.Get("Content-Type")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:  "ct-rule",
			Match: models.MatcherConfig{},
			Webhook: models.WebhookConfig{
				URL:    srv.URL,
				Method: "POST",
				Headers: map[string]string{
					"Content-Type": "application/x-custom+json",
				},
			},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "CT test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, &calls, 1, 3*time.Second)

	mu.Lock()
	ct := capturedCT
	mu.Unlock()

	if ct != "application/x-custom+json" {
		t.Errorf("Content-Type = %q, want application/x-custom+json", ct)
	}
}

// TestE2E_PartialFailureMultipleRules verifies that when one rule's webhook
// permanently fails but another succeeds, the second webhook still receives
// its delivery.
func TestE2E_PartialFailureMultipleRules(t *testing.T) {
	// Always-failing webhook.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(failSrv.Close)

	// Always-succeeding webhook.
	successURL, successCalls, _ := webhookServer(t)

	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "fail-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: failSrv.URL, Method: "POST"},
		},
		{
			Name:    "success-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: successURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Partial failure", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	// The successful webhook must still fire despite the other rule failing.
	waitForCalls(t, successCalls, 1, 5*time.Second)
	if successCalls.Load() != 1 {
		t.Errorf("success-rule: expected 1 call, got %d", successCalls.Load())
	}
}

// --- Rule Variation Tests ---

// TestE2E_FromDomainWildcard verifies that a wildcard from_domain pattern
// (*.example.com) correctly matches emails from subdomains.
func TestE2E_FromDomainWildcard(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "subdomain-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "from_domain", Pattern: "*.example.com"}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// Match: sender is from a subdomain.
	if err := sendEmail(t, h.smtpAddr, "alert@monitor.example.com", []string{"r@dest.com"},
		rawMessage("alert@monitor.example.com", "r@dest.com", "Subdomain match", "body")); err != nil {
		t.Fatalf("sendEmail (match): %v", err)
	}
	waitForCalls(t, calls, 1, 3*time.Second)

	// No match: sender is at the apex domain itself.
	if err := sendEmail(t, h.smtpAddr, "alert@example.com", []string{"r@dest.com"},
		rawMessage("alert@example.com", "r@dest.com", "Apex no match", "body")); err != nil {
		t.Fatalf("sendEmail (no match): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if calls.Load() != 1 {
		t.Errorf("expected 1 call (subdomain match only), got %d", calls.Load())
	}
}

// TestE2E_SubjectRegexCombinedWithFromDomain verifies AND logic combining a
// regex subject pattern with a glob from_domain pattern.
func TestE2E_SubjectRegexCombinedWithFromDomain(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name: "combined-rule",
			Match: models.MatcherConfig{Conditions: []models.MatchCondition{
				{Field: "from_domain", Pattern: "*.infra.io"},
				{Field: "subject", Pattern: `/^(CRIT|WARN):/`},
			}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// Match: both conditions satisfied.
	if err := sendEmail(t, h.smtpAddr, "mon@alerts.infra.io", []string{"ops@company.com"},
		rawMessage("mon@alerts.infra.io", "ops@company.com", "CRIT: disk full", "body")); err != nil {
		t.Fatalf("sendEmail (match): %v", err)
	}
	waitForCalls(t, calls, 1, 3*time.Second)

	// No match: subject doesn't match regex.
	if err := sendEmail(t, h.smtpAddr, "mon@alerts.infra.io", []string{"ops@company.com"},
		rawMessage("mon@alerts.infra.io", "ops@company.com", "INFO: system ok", "body")); err != nil {
		t.Fatalf("sendEmail (no match): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	// No match: domain doesn't match wildcard.
	if err := sendEmail(t, h.smtpAddr, "mon@other.com", []string{"ops@company.com"},
		rawMessage("mon@other.com", "ops@company.com", "CRIT: something", "body")); err != nil {
		t.Fatalf("sendEmail (no match domain): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 call (AND logic), got %d", calls.Load())
	}
}

// TestE2E_ToEmailRegexMultipleRecipients verifies regex to_email matching with
// OR logic: if any recipient matches the pattern, the rule fires.
func TestE2E_ToEmailRegexMultipleRecipients(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "pagerduty-rule",
			Match:   models.MatcherConfig{Conditions: []models.MatchCondition{{Field: "rcpt_to", Pattern: `/^(oncall|pagerduty)@/`}}},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// One of two recipients matches.
	if err := sendEmail(t, h.smtpAddr, "sender@foo.com",
		[]string{"dev@company.com", "oncall@company.com"},
		rawMessage("sender@foo.com", "dev@company.com", "Incident", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}
	waitForCalls(t, calls, 1, 3*time.Second)
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

// --- Payload Variation Tests ---

// TestE2E_EmailWithAttachment verifies that an attachment is present in the
// webhook payload with correct filename and base64-encoded content.
func TestE2E_EmailWithAttachment(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "attachment-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	attachmentMsg := "From: sender@example.com\r\n" +
		"To: r@dest.com\r\n" +
		"Subject: Has attachment\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"bound99\"\r\n" +
		"\r\n" +
		"--bound99\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"\r\n" +
		"See the attached file.\r\n" +
		"--bound99\r\n" +
		"Content-Type: text/plain; name=\"report.txt\"\r\n" +
		"Content-Disposition: attachment; filename=\"report.txt\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"SGVsbG8gZnJvbSBhdHRhY2htZW50\r\n" + // base64("Hello from attachment")
		"--bound99--\r\n"

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"}, attachmentMsg); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}

	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, bs[0])
	}

	if !strings.Contains(parsed.TextBody, "See the attached file") {
		t.Errorf("TextBody = %q, expected body text", parsed.TextBody)
	}
	if len(parsed.Attachments) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(parsed.Attachments))
	}
	att := parsed.Attachments[0]
	if att.Filename != "report.txt" {
		t.Errorf("Attachment.Filename = %q, want report.txt", att.Filename)
	}
	if att.Content == "" {
		t.Error("Attachment.Content is empty, expected base64 data")
	}
}

// TestE2E_AuthResultInPayload verifies that the auth_result field is present
// in the webhook JSON payload when auth mode is "log".
func TestE2E_AuthResultInPayload(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarnessWithAuth(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeLog,
		DKIM:  models.AuthModeOff,
		DMARC: models.AuthModeOff,
	})
	h.engine.SetRules([]models.Rule{
		{
			Name:    "auth-payload-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Auth payload test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 5*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}

	var parsed models.ParsedEmail
	if err := json.Unmarshal(bs[0], &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.AuthResult == nil {
		t.Fatal("expected non-nil AuthResult in payload when SPF=log")
	}
	// SPF was checked (log mode). For test domains without real SPF records the
	// result is either "pass", "none", or "fail" — the key requirement is that
	// the field is present and has a valid value.
	validSPF := parsed.AuthResult.SPF == models.AuthPass ||
		parsed.AuthResult.SPF == models.AuthNone ||
		parsed.AuthResult.SPF == models.AuthFail
	if !validSPF {
		t.Errorf("AuthResult.SPF = %q, want a valid AuthCheckResult", parsed.AuthResult.SPF)
	}
}

// TestE2E_CustomTemplateWithAttachments verifies template rendering when
// the email contains both text body and attachments.
func TestE2E_CustomTemplateWithAttachments(t *testing.T) {
	hookURL, calls, bodies := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:  "tmpl-attach-rule",
			Match: models.MatcherConfig{},
			Webhook: models.WebhookConfig{
				URL:             hookURL,
				Method:          "POST",
				PayloadTemplate: `{"from":{{.From}},"has_attachments":{{.Attachments}}}`,
			},
		},
	})

	attachmentMsg := "From: sender@example.com\r\n" +
		"To: r@dest.com\r\n" +
		"Subject: Template attach\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"tbnd\"\r\n" +
		"\r\n" +
		"--tbnd\r\n" +
		"Content-Type: text/plain\r\n" +
		"\r\n" +
		"Body text\r\n" +
		"--tbnd\r\n" +
		"Content-Type: application/octet-stream; name=\"data.bin\"\r\n" +
		"Content-Disposition: attachment; filename=\"data.bin\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"AQID\r\n" +
		"--tbnd--\r\n"

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"}, attachmentMsg); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	waitForCalls(t, calls, 1, 3*time.Second)

	bs := bodies()
	if len(bs) == 0 {
		t.Fatal("no bodies captured")
	}

	var result map[string]json.RawMessage
	if err := json.Unmarshal(bs[0], &result); err != nil {
		t.Fatalf("unmarshal custom template body: %v\nbody: %s", err, bs[0])
	}
	if _, ok := result["has_attachments"]; !ok {
		t.Error("expected 'has_attachments' key in template output")
	}
	// Attachments should be a non-null JSON array.
	if string(result["has_attachments"]) == "null" {
		t.Error("expected non-null attachments array in template output")
	}
}

// TestE2E_HotReloadClearRules verifies that removing all rules via SetRules([])
// stops webhook delivery immediately for subsequent emails.
func TestE2E_HotReloadClearRules(t *testing.T) {
	hookURL, calls, _ := webhookServer(t)
	h := newTestHarness(t, smtpCfg(t))
	h.engine.SetRules([]models.Rule{
		{
			Name:    "active-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	// First email should fire the webhook.
	if err := sendEmail(t, h.smtpAddr, "a@example.com", []string{"r@dest.com"},
		rawMessage("a@example.com", "r@dest.com", "Before clear", "body")); err != nil {
		t.Fatalf("sendEmail (before clear): %v", err)
	}
	waitForCalls(t, calls, 1, 3*time.Second)

	// Clear all rules.
	h.engine.SetRules(nil)

	// Subsequent emails should be dropped silently.
	if err := sendEmail(t, h.smtpAddr, "b@example.com", []string{"r@dest.com"},
		rawMessage("b@example.com", "r@dest.com", "After clear", "body")); err != nil {
		t.Fatalf("sendEmail (after clear): %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	if calls.Load() != 1 {
		t.Errorf("expected exactly 1 call (before clear only), got %d", calls.Load())
	}
}

// TestE2E_WebhookResponseBodyCaptured verifies that the webhook response body
// is accessible when the server returns a 4xx (non-retryable) error.
func TestE2E_WebhookResponseBodyCaptured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"error":"validation_failed","field":"subject"}`))
	}))
	t.Cleanup(srv.Close)

	// Dispatch directly through the dispatcher (no SMTP needed for this test).
	d := webhook.NewDispatcher(models.RetryConfig{
		MaxRetries:     0,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        5 * time.Second,
		RetryOnTimeout: true,
	}, "dev")
	email := &models.ParsedEmail{
		EnvelopeFrom: "s@example.com",
		Subject:      "Validation test",
	}
	rule := models.Rule{
		Name:    "validation-rule",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}

	results := d.Dispatch(context.Background(), email, []models.Rule{rule})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	res := results[0]
	if res.Status != "rejected" {
		t.Errorf("Status = %q, want rejected", res.Status)
	}
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("StatusCode = %d, want 422", res.StatusCode)
	}
	if !strings.Contains(res.ResponseBody, "validation_failed") {
		t.Errorf("ResponseBody = %q, expected to contain 'validation_failed'", res.ResponseBody)
	}
}

// TestE2E_StoreTracksDeliveryResults verifies end-to-end that a delivered email
// results in a corresponding delivery record in the database with correct status.
func TestE2E_StoreTracksDeliveryResults(t *testing.T) {
	hookURL, _, _ := webhookServer(t)
	h, store := newTestHarnessWithStore(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeOff,
		DKIM:  models.AuthModeOff,
		DMARC: models.AuthModeOff,
	})
	h.engine.SetRules([]models.Rule{
		{
			Name:    "store-track-rule",
			Match:   models.MatcherConfig{},
			Webhook: models.WebhookConfig{URL: hookURL, Method: "POST"},
		},
	})

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Store tracking test", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	// Poll until the email appears with delivered status.
	ctx := context.Background()
	deadline := time.Now().Add(5 * time.Second)
	var emailID string
	for time.Now().Before(deadline) {
		result, err := store.ListEmails(ctx, storage.EmailFilter{Status: "delivered"})
		if err != nil {
			t.Fatalf("ListEmails: %v", err)
		}
		if result.Total > 0 {
			emailID = result.Emails[0].ID
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if emailID == "" {
		t.Fatal("timed out waiting for email with delivered status in DB")
	}

	deliveries, err := store.GetDeliveriesForEmail(ctx, emailID)
	if err != nil {
		t.Fatalf("GetDeliveriesForEmail: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery record, got %d", len(deliveries))
	}
	d := deliveries[0]
	if d.Status != "success" {
		t.Errorf("Delivery.Status = %q, want success", d.Status)
	}
	if d.RuleName != "store-track-rule" {
		t.Errorf("Delivery.RuleName = %q, want store-track-rule", d.RuleName)
	}
	if d.Attempts < 1 {
		t.Errorf("Delivery.Attempts = %d, want >= 1", d.Attempts)
	}
}

// TestE2E_StoreTracksDroppedEmail verifies that an email with no matching rule
// is stored with status=dropped in the database.
func TestE2E_StoreTracksDroppedEmail(t *testing.T) {
	h, store := newTestHarnessWithStore(t, smtpCfg(t), models.AuthConfig{
		SPF:   models.AuthModeOff,
		DKIM:  models.AuthModeOff,
		DMARC: models.AuthModeOff,
	})
	// No rules → every email is dropped.
	_ = h

	if err := sendEmail(t, h.smtpAddr, "sender@example.com", []string{"r@dest.com"},
		rawMessage("sender@example.com", "r@dest.com", "Should be dropped", "body")); err != nil {
		t.Fatalf("sendEmail: %v", err)
	}

	ctx := context.Background()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		result, err := store.ListEmails(ctx, storage.EmailFilter{Status: "dropped"})
		if err != nil {
			t.Fatalf("ListEmails: %v", err)
		}
		if result.Total > 0 {
			return // success
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("timed out waiting for email with dropped status in DB")
}
