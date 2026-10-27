package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/httpcsrf"
	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/storage"
)

// startRun runs mailrelay with the given config file contents and returns the
// HTTP base URL and a function that stops it and returns run's error.
func startRun(t *testing.T, httpAddr, cfgYAML string) (baseURL string, stopRun func() error) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", cfgPath}, io.Discard) }()
	t.Cleanup(func() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) })

	baseURL = "http://" + httpAddr

	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		select {
		case err := <-done:
			cancel()
			t.Fatalf("run exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	var stopped bool
	stopRun = func() error {
		if stopped {
			return nil
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			return fmt.Errorf("run did not stop")
		}
	}
	t.Cleanup(func() { stopRun() })
	return baseURL, stopRun
}

func TestRun_Version(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "mailrelay dev") {
		t.Errorf("version output = %q", out.String())
	}
}

func TestRun_Errors(t *testing.T) {
	if err := run(context.Background(), []string{"-nope"}, io.Discard); err == nil {
		t.Error("expected error for unknown flag")
	}

	dir := t.TempDir()
	write := func(name, contents string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(contents), 0o600)
		return p
	}

	cases := map[string]string{
		"invalid auth mode": "auth:\n  spf: sometimes\n",
		"webui without db":  "webui:\n  enabled: true\n  db_path: \"\"\n",
		"bad db path":       fmt.Sprintf("webui:\n  enabled: true\n  db_path: %q\n", filepath.Join(dir, "missing", "dir", "db.sqlite")),
		"auth without providers": fmt.Sprintf("smtp:\n  addr: %q\nhttp:\n  addr: %q\n  auth:\n    enabled: true\nwebui:\n  db_path: %q\n",
			freePort(t), freePort(t), filepath.Join(dir, "db.sqlite")),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			path := write(strings.ReplaceAll(name, " ", "_")+".yaml", cfg)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := run(ctx, []string{"-config", path}, io.Discard); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestRun_FullStack(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "mailrelay.db")
	rawDir := filepath.Join(dir, "raw")

	// Settings saved through the UI are applied over the config file at startup.
	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	saved := &models.AppConfig{
		LogLevel: "debug",
		Auth:     models.AuthConfig{SPF: models.AuthModeOff, DKIM: models.AuthModeEnforce, DMARC: models.AuthModeOff},
		Retry:    models.RetryConfig{MaxRetries: 0, InitialWait: time.Second, MaxWait: time.Second, Timeout: 5 * time.Second},
		WebUI:    models.WebUIConfig{RetentionDays: 30},
	}
	if err := store.SaveSettings(context.Background(), saved); err != nil {
		t.Fatal(err)
	}
	store.Close()

	smtpAddr, httpAddr := freePort(t), freePort(t)
	baseURL, stopRun := startRun(t, httpAddr, fmt.Sprintf(`
log_level: info
smtp:
  addr: %q
  domain: test.local
http:
  addr: %q
webhook:
  allow_private_networks: true
auth:
  spf: "off"
  dkim: "off"
  dmarc: "off"
webui:
  enabled: true
  db_path: %q
  raw_email_dir: %q
  retention_days: 7
`, smtpAddr, httpAddr, dbPath, rawDir))

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d", resp.StatusCode)
	}
	for header, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff"} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	var csrfToken string
	u, _ := url.Parse(baseURL)
	for _, c := range jar.Cookies(u) {
		if c.Name == httpcsrf.CookieName {
			csrfToken = c.Value
		}
	}
	if csrfToken == "" {
		t.Fatal("no CSRF cookie set")
	}

	// An email with no rules is dropped.
	if err := sendEmail(t, smtpAddr, "a@example.com", []string{"b@example.com"}, rawMessage("a@example.com", "b@example.com", "No rules yet", "hi")); err != nil {
		t.Fatalf("send: %v", err)
	}

	// POSTs without a CSRF token are refused.
	resp, err = client.PostForm(baseURL+"/rules", url.Values{"name": {"x"}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST without CSRF status = %d", resp.StatusCode)
	}

	hookURL, calls, _ := webhookServer(t)
	resp, err = client.PostForm(baseURL+"/rules", url.Values{
		"csrf_token":  {csrfToken},
		"name":        {"everything"},
		"webhook_url": {hookURL},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create rule status = %d", resp.StatusCode)
	}

	if err := sendEmail(t, smtpAddr, "a@example.com", []string{"b@example.com"}, rawMessage("a@example.com", "b@example.com", "Matched", "hi")); err != nil {
		t.Fatalf("send: %v", err)
	}
	waitForCalls(t, calls, 1, 5*time.Second)

	// DKIM is enforced via the saved settings, so a bad signature is rejected and recorded.
	if err := sendEmail(t, smtpAddr, "a@example.com", []string{"b@example.com"}, invalidDKIMMessage("a@example.com", "b@example.com", "Bad DKIM")); err == nil {
		t.Error("expected DKIM rejection")
	}

	var stats struct{ Total, Delivered, Dropped, Rejected int }
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL + "/api/stats")
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&stats)
		resp.Body.Close()
		if stats.Total == 3 && stats.Delivered == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stats.Total != 3 || stats.Delivered != 1 || stats.Dropped != 1 || stats.Rejected != 1 {
		t.Errorf("stats = %+v", stats)
	}

	entries, _ := os.ReadDir(rawDir)
	if len(entries) != 3 {
		t.Errorf("raw emails = %d, want 3", len(entries))
	}

	resp, err = client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("metrics status = %d", resp.StatusCode)
	}

	if err := stopRun(); err != nil {
		t.Errorf("run returned %v", err)
	}
}

func TestRun_WithoutWebUI(t *testing.T) {
	smtpAddr, httpAddr := freePort(t), freePort(t)
	baseURL, stopRun := startRun(t, httpAddr, fmt.Sprintf("smtp:\n  addr: %q\nhttp:\n  addr: %q\nwebui:\n  enabled: false\n", smtpAddr, httpAddr))

	if err := sendEmail(t, smtpAddr, "a@example.com", []string{"b@example.com"}, rawMessage("a@example.com", "b@example.com", "s", "b")); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET / without web UI status = %d", resp.StatusCode)
	}
	if err := stopRun(); err != nil {
		t.Errorf("run returned %v", err)
	}
}

func TestRun_AuthEnabled(t *testing.T) {
	for _, protectMetrics := range []bool{false, true} {
		t.Run(fmt.Sprintf("protect_metrics=%v", protectMetrics), func(t *testing.T) {
			dir := t.TempDir()
			smtpAddr, httpAddr := freePort(t), freePort(t)
			baseURL, _ := startRun(t, httpAddr, fmt.Sprintf(`
smtp:
  addr: %q
http:
  addr: %q
  protect_metrics: %v
  auth:
    enabled: true
    session_secret: test-secret
    providers:
      - name: google
        client_id: id
        client_secret: secret
webui:
  enabled: true
  db_path: %q
`, smtpAddr, httpAddr, protectMetrics, filepath.Join(dir, "db.sqlite")))

			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			status := func(path string) int {
				t.Helper()
				resp, err := client.Get(baseURL + path)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				return resp.StatusCode
			}

			if got := status("/healthz"); got != http.StatusOK {
				t.Errorf("/healthz = %d", got)
			}
			for _, path := range []string{"/", "/static/app.js", "/auth/login"} {
				if got := status(path); got != http.StatusFound {
					t.Errorf("%s = %d, want login redirect", path, got)
				}
			}
			wantMetrics := http.StatusOK
			if protectMetrics {
				wantMetrics = http.StatusFound
			}
			if got := status("/metrics"); got != wantMetrics {
				t.Errorf("/metrics = %d, want %d", got, wantMetrics)
			}
			if got := status("/auth/logout"); got != http.StatusSeeOther {
				t.Errorf("/auth/logout = %d", got)
			}
			if got := status("/auth/callback"); got != http.StatusBadRequest {
				t.Errorf("/auth/callback without code = %d", got)
			}
		})
	}
}

func TestCleanupOnce(t *testing.T) {
	dir := t.TempDir()
	store, err := storage.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	old := &models.EmailRecord{ID: "old", ReceivedAt: time.Now().Add(-10 * 24 * time.Hour), Status: models.EmailStatusDelivered}
	recent := &models.EmailRecord{ID: "recent", Status: models.EmailStatusDelivered}
	for _, rec := range []*models.EmailRecord{old, recent} {
		if err := store.SaveEmail(ctx, rec); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, rec.ID+".eml"), []byte("x"), 0o600)
	}

	cleanupOnce(ctx, store, 0, dir)
	if ids, _ := store.AllEmailIDs(ctx); len(ids) != 2 {
		t.Fatalf("retention 0 deleted emails: %v", ids)
	}

	cleanupOnce(ctx, store, 7, dir)
	ids, _ := store.AllEmailIDs(ctx)
	if len(ids) != 1 || ids[0] != "recent" {
		t.Errorf("remaining = %v", ids)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.eml")); !os.IsNotExist(err) {
		t.Error("old raw email not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "recent.eml")); err != nil {
		t.Error("recent raw email removed")
	}
}

func TestRunCleanupStops(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var days atomic.Int32
	days.Store(7)
	go func() {
		runCleanup(ctx, store, &days, "")
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runCleanup did not stop")
	}
}

func TestSetLogLevel(t *testing.T) {
	lv := new(slog.LevelVar)
	for level, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError, "": slog.LevelInfo,
	} {
		setLogLevel(lv, level)
		if lv.Level() != want {
			t.Errorf("setLogLevel(%q) = %v, want %v", level, lv.Level(), want)
		}
	}
}
