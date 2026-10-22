package webhook

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestIsInternalIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"192.168.1.10", true},
		{"169.254.169.254", true}, // cloud metadata
		{"100.100.100.100", true}, // CGNAT / Tailscale
		{"0.0.0.0", true},
		{"224.0.0.1", true},
		{"::1", true},
		{"fd00::1", true},
		{"fe80::1", true},
		{"::ffff:192.168.1.1", true}, // IPv4-mapped
		{"8.8.8.8", false},
		{"172.32.0.1", false},
		{"100.128.0.1", false},
		{"2606:4700:4700::1111", false},
	}
	for _, tt := range tests {
		if got := isInternalIP(net.ParseIP(tt.ip)); got != tt.want {
			t.Errorf("isInternalIP(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestDispatchOneBlocksPrivateNetworks(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Add(1)
	}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{MaxRetries: 3, InitialWait: 10 * time.Millisecond, MaxWait: 50 * time.Millisecond, Timeout: 5 * time.Second}, "dev")
	d.SetAllowPrivateNetworks(false)
	rule := models.Rule{Name: "test", Webhook: models.WebhookConfig{URL: srv.URL, Method: "POST"}}

	result := d.dispatchOne(context.Background(), &models.ParsedEmail{Subject: "Test"}, rule)
	if called.Load() != 0 {
		t.Errorf("blocked webhook was called %d times", called.Load())
	}
	if result.Status != "rejected" || result.Attempts != 1 {
		t.Errorf("status = %q after %d attempts, want rejected after 1 (no retries)", result.Status, result.Attempts)
	}

	d.SetAllowPrivateNetworks(true)
	result = d.dispatchOne(context.Background(), &models.ParsedEmail{Subject: "Test"}, rule)
	if result.Status != "success" || called.Load() != 1 {
		t.Errorf("status = %q, calls = %d; want success with 1 call once allowed", result.Status, called.Load())
	}
}

func TestConnectivityBlocksPrivateNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	d := NewDispatcher(models.RetryConfig{}, "dev")
	d.SetAllowPrivateNetworks(false)

	if _, err := d.TestConnectivity(context.Background(), srv.URL, "POST"); !errors.Is(err, ErrPrivateAddress) {
		t.Errorf("TestConnectivity error = %v, want ErrPrivateAddress", err)
	}
}
