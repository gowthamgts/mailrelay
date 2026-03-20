package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestBackoff(t *testing.T) {
	retry := models.RetryConfig{
		InitialWait: 1 * time.Second,
		MaxWait:     10 * time.Second,
	}

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},  // 1s * 2^0
		{2, 2 * time.Second},  // 1s * 2^1
		{3, 4 * time.Second},  // 1s * 2^2
		{4, 8 * time.Second},  // 1s * 2^3
		{5, 10 * time.Second}, // capped at MaxWait
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			got := backoff(retry, tt.attempt)
			if got != tt.want {
				t.Errorf("backoff(%d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestBuildPayload(t *testing.T) {
	email := &models.ParsedEmail{
		From:    "sender@example.com",
		Subject: "Test",
	}

	t.Run("default JSON", func(t *testing.T) {
		wh := models.WebhookConfig{}
		payload, err := buildPayload(email, wh)
		if err != nil {
			t.Fatalf("buildPayload() error: %v", err)
		}
		if len(payload) == 0 {
			t.Fatal("expected non-empty payload")
		}
		// Should be valid JSON containing the subject.
		if got := string(payload); !containsStr(got, `"subject":"Test"`) {
			t.Errorf("payload missing subject: %s", got)
		}
	})

	t.Run("custom template", func(t *testing.T) {
		// Template variables are pre-encoded JSON values (rawJSON), so string
		// fields already include surrounding quotes. Use {{.From}} without extra
		// quotes in the template.
		wh := models.WebhookConfig{
			PayloadTemplate: `{"from":{{.From}},"subj":{{.Subject}}}`,
		}
		payload, err := buildPayload(email, wh)
		if err != nil {
			t.Fatalf("buildPayload() error: %v", err)
		}
		expected := `{"from":"sender@example.com","subj":"Test"}`
		if string(payload) != expected {
			t.Errorf("payload = %q, want %q", string(payload), expected)
		}
	})

	t.Run("invalid template", func(t *testing.T) {
		wh := models.WebhookConfig{
			PayloadTemplate: "{{.Invalid",
		}
		_, err := buildPayload(email, wh)
		if err == nil {
			t.Fatal("expected error for invalid template")
		}
	})
}

func TestDispatchOneSuccess(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond, Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	result := d.dispatchOne(context.Background(), email, rule)
	if called.Load() != 1 {
		t.Errorf("expected 1 call, got %d", called.Load())
	}
	if result.ResponseBody != `{"ok":true}` {
		t.Errorf("ResponseBody = %q, want %q", result.ResponseBody, `{"ok":true}`)
	}
}

func TestDispatchOneRetryOn5xx(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := called.Add(1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond, Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	d.dispatchOne(context.Background(), email, rule)
	if called.Load() != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 retries), got %d", called.Load())
	}
}

func TestDispatchOneNoRetryOn4xx(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond, Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	d.dispatchOne(context.Background(), email, rule)
	if called.Load() != 1 {
		t.Errorf("expected 1 call (no retry on 4xx), got %d", called.Load())
	}
}

func TestDispatchOneContextCancellation(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{MaxRetries: 5, InitialWait: 100 * time.Millisecond, MaxWait: 1 * time.Second, Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first attempt to prevent retries.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	d.dispatchOne(ctx, email, rule)
	if called.Load() > 2 {
		t.Errorf("expected at most 2 calls with cancelled context, got %d", called.Load())
	}
}

func TestPerWebhookTimeoutOverride(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{MaxRetries: 0, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond, Timeout: 1 * time.Millisecond, RetryOnTimeout: false}, "dev")
	// Per-webhook timeout overrides the tiny global timeout.
	whTimeout := 5
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST", Timeout: &whTimeout},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	result := d.dispatchOne(context.Background(), email, rule)
	if result.Status != "success" {
		t.Errorf("expected success with per-webhook timeout override, got %q: %s", result.Status, result.Error)
	}
	if called.Load() != 1 {
		t.Errorf("expected 1 call, got %d", called.Load())
	}
}

func TestPerWebhookRetryOverride(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := called.Add(1)
		if n <= 5 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Global allows 1 retry, but per-webhook allows 5.
	d := NewDispatcher(models.RetryConfig{MaxRetries: 1, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond, Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
	whRetries := 5
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST", MaxRetries: &whRetries},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	result := d.dispatchOne(context.Background(), email, rule)
	if result.Status != "success" {
		t.Errorf("expected success with per-webhook retry override, got %q: %s", result.Status, result.Error)
	}
	// 6 calls: 1 initial + 5 retries
	if called.Load() != 6 {
		t.Errorf("expected 6 calls, got %d", called.Load())
	}
}

func TestRetryOnTimeoutFalse(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
		// Simulate a slow response that triggers timeout.
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{
		MaxRetries:     3,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        50 * time.Millisecond,
		RetryOnTimeout: false,
	}, "dev")
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	result := d.dispatchOne(context.Background(), email, rule)
	if result.Status != "failed" {
		t.Errorf("expected failed, got %q", result.Status)
	}
	// Should stop after 1 attempt — no retries on timeout.
	if called.Load() != 1 {
		t.Errorf("expected 1 call (no retry on timeout), got %d", called.Load())
	}
}

func TestRetryOnTimeoutTrue(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := called.Add(1)
		if n <= 2 {
			// Slow response → timeout
			time.Sleep(200 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{
		MaxRetries:     3,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        50 * time.Millisecond,
		RetryOnTimeout: true,
	}, "dev")
	rule := models.Rule{
		Name:    "test",
		Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"},
	}
	email := &models.ParsedEmail{Subject: "Test"}

	result := d.dispatchOne(context.Background(), email, rule)
	if result.Status != "success" {
		t.Errorf("expected success, got %q: %s", result.Status, result.Error)
	}
	if called.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", called.Load())
	}
}

func TestIsTimeoutError(t *testing.T) {
	if !isTimeoutError(context.DeadlineExceeded) {
		t.Error("expected context.DeadlineExceeded to be a timeout error")
	}
	if isTimeoutError(context.Canceled) {
		t.Error("expected context.Canceled to NOT be a timeout error")
	}
	if isTimeoutError(nil) {
		t.Error("expected nil to NOT be a timeout error")
	}
}

func TestTestConnectivity(t *testing.T) {
	t.Run("reachable", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		d := NewDispatcher(models.RetryConfig{Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
		code, err := d.TestConnectivity(context.Background(), srv.URL, "POST")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if code != 200 {
			t.Errorf("expected 200, got %d", code)
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		d := NewDispatcher(models.RetryConfig{Timeout: 5 * time.Second, RetryOnTimeout: true}, "dev")
		_, err := d.TestConnectivity(context.Background(), "http://192.0.2.1:1", "POST")
		if err == nil {
			t.Fatal("expected error for unreachable host")
		}
	})
}

func TestEffectiveHelpers(t *testing.T) {
	timeout := 10
	maxRetries := 5
	initialWait := 2
	maxWait := 60
	retryOnTimeout := false

	wh := models.WebhookConfig{
		URL:            "http://example.com",
		Timeout:        &timeout,
		MaxRetries:     &maxRetries,
		InitialWait:    &initialWait,
		MaxWait:        &maxWait,
		RetryOnTimeout: &retryOnTimeout,
	}

	if got := wh.EffectiveTimeout(30 * time.Second); got != 10*time.Second {
		t.Errorf("EffectiveTimeout = %v, want 10s", got)
	}
	if got := wh.EffectiveMaxRetries(3); got != 5 {
		t.Errorf("EffectiveMaxRetries = %d, want 5", got)
	}
	if got := wh.EffectiveInitialWait(1 * time.Second); got != 2*time.Second {
		t.Errorf("EffectiveInitialWait = %v, want 2s", got)
	}
	if got := wh.EffectiveMaxWait(30 * time.Second); got != 60*time.Second {
		t.Errorf("EffectiveMaxWait = %v, want 60s", got)
	}
	if got := wh.EffectiveRetryOnTimeout(true); got != false {
		t.Errorf("EffectiveRetryOnTimeout = %v, want false", got)
	}

	// Nil overrides should fall back to global.
	wh2 := models.WebhookConfig{URL: "http://example.com"}
	if got := wh2.EffectiveTimeout(30 * time.Second); got != 30*time.Second {
		t.Errorf("EffectiveTimeout (nil) = %v, want 30s", got)
	}
	if got := wh2.EffectiveMaxRetries(3); got != 3 {
		t.Errorf("EffectiveMaxRetries (nil) = %d, want 3", got)
	}
	if got := wh2.EffectiveRetryOnTimeout(true); got != true {
		t.Errorf("EffectiveRetryOnTimeout (nil) = %v, want true", got)
	}
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}
