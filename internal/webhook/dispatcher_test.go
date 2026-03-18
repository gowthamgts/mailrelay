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

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}, "dev")
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

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}, "dev")
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

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond}, "dev")
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

	d := NewDispatcher(models.RetryConfig{MaxRetries: 5, InitialWait: 100 * time.Millisecond, MaxWait: 1 * time.Second}, "dev")
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

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}
