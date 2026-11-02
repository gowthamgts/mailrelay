package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func checkSignature(t *testing.T, r *http.Request, secret string) {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Error(err)
		return
	}
	ts := r.Header.Get("X-Mailrelay-Timestamp")
	seconds, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(seconds, 0)).Abs() > 5*time.Minute {
		t.Errorf("invalid timestamp %q", ts)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(append([]byte(ts+"."), body...))
	if got, want := r.Header.Get("X-Mailrelay-Signature"), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("signature = %q, want %q", got, want)
	}
	if r.Header.Get("X-Mailrelay-Delivery") == "" {
		t.Error("missing delivery ID")
	}
}

func TestSignedDispatch(t *testing.T) {
	for _, template := range []string{"", " {\"text\": {{.TextBody}}} \n"} {
		t.Run(template, func(t *testing.T) {
			calls := 0
			var firstTimestamp, deliveryID string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checkSignature(t, r, "secret")
				calls++
				if calls == 1 {
					firstTimestamp = r.Header.Get("X-Mailrelay-Timestamp")
					deliveryID = r.Header.Get("X-Mailrelay-Delivery")
					w.WriteHeader(500)
					return
				}
				if r.Header.Get("X-Mailrelay-Timestamp") == firstTimestamp {
					t.Error("retry reused timestamp")
				}
				if r.Header.Get("X-Mailrelay-Delivery") != deliveryID {
					t.Error("retry changed delivery ID")
				}
			}))
			defer srv.Close()
			d := NewDispatcher(models.RetryConfig{MaxRetries: 1, InitialWait: time.Second, MaxWait: time.Second, Timeout: 5 * time.Second}, "test")
			rule := models.Rule{Name: "signed", Webhook: models.WebhookConfig{URL: srv.URL, SigningSecret: "secret", PayloadTemplate: template, Headers: map[string]string{"X-Mailrelay-Signature": "override", "X-Mailrelay-Timestamp": "0"}}}
			result := d.Dispatch(context.Background(), &models.ParsedEmail{TextBody: "hello\n世界"}, []models.Rule{rule})
			if result[0].Status != "success" || calls != 2 {
				t.Fatalf("result = %+v, calls = %d", result, calls)
			}
		})
	}
}

func TestSigningConnectivityAndUnsigned(t *testing.T) {
	for _, secret := range []string{"", "test-secret"} {
		t.Run(secret, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if secret != "" {
					checkSignature(t, r, secret)
				} else {
					for _, header := range []string{"X-Mailrelay-Timestamp", "X-Mailrelay-Signature", "X-Mailrelay-Delivery"} {
						if r.Header.Get(header) != "" {
							t.Errorf("unsigned request has %s", header)
						}
					}
				}
			}))
			defer srv.Close()
			d := NewDispatcher(models.RetryConfig{Timeout: time.Second}, "test")
			if _, err := d.TestConnectivity(context.Background(), srv.URL, "POST", secret); err != nil {
				t.Fatal(err)
			}
			results := d.Dispatch(context.Background(), &models.ParsedEmail{}, []models.Rule{{Webhook: models.WebhookConfig{URL: srv.URL, SigningSecret: secret}}})
			if results[0].Status != "success" {
				t.Fatal(results)
			}
		})
	}
}

func TestSignedRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("followed signed redirect") }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer source.Close()
	d := NewDispatcher(models.RetryConfig{Timeout: time.Second}, "test")
	result, err := d.TestConnectivity(context.Background(), source.URL, "POST", "secret")
	if err != nil || result.StatusCode != http.StatusFound {
		t.Fatalf("result %+v, err %v", result, err)
	}
}
