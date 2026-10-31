package webhook

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/storage"
)

func awaitQueue(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for delivery state")
}

func saveQueueEmail(t *testing.T, s *storage.Store) string {
	t.Helper()
	rec := &models.EmailRecord{Status: models.EmailStatusReceived, ParsedEmail: &models.ParsedEmail{Subject: "persist me"}, ReceivedAt: time.Now().Add(-72 * time.Hour)}
	if err := s.SaveEmail(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func stopQueue(t *testing.T, d *Dispatcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestQueueRestartResume(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.db")
	s, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var clock atomic.Int64
	clock.Store(time.Now().UTC().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("resumed"))
	}))
	defer server.Close()
	retry := models.RetryConfig{MaxRetries: 2, InitialWait: time.Hour, MaxWait: 2 * time.Hour, Timeout: time.Second, RetryOnTimeout: true}
	d := NewDispatcher(retry, "test")
	d.now = now
	id := saveQueueEmail(t, s)
	if err := d.Enqueue(ctx, s, id, &models.ParsedEmail{Subject: "persist me"}, []models.Rule{{Name: "resume", Webhook: models.WebhookConfig{URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	if err := d.StartQueue(s, 1, nil); err != nil {
		t.Fatal(err)
	}
	awaitQueue(t, func() bool {
		rows, _ := s.GetDeliveriesForEmail(ctx, id)
		return len(rows) == 1 && rows[0].Status == "pending" && rows[0].Attempts == 1
	})
	stopQueue(t, d)
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if count, err := s.Cleanup(ctx, 1); err != nil || count != 0 {
		t.Fatalf("retention removed unfinished email: %d, %v", count, err)
	}
	ids, err := s.GetExpiredEmailIDs(ctx, 1)
	if err != nil || len(ids) != 0 {
		t.Fatalf("raw email retention: %v, %v", ids, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// New defaults must not alter the snapshotted retry settings.
	d = NewDispatcher(models.RetryConfig{}, "test")
	d.now = now
	if err := d.StartQueue(s, 2, nil); err != nil {
		t.Fatal(err)
	}
	defer stopQueue(t, d)
	// Verify the persisted deadline without relying on wall-clock backoff sleeps.
	clock.Add(int64(time.Hour - time.Nanosecond))
	job, err := s.ClaimDelivery(ctx, now())
	if err != nil || job != nil {
		t.Fatalf("claimed before due: %+v, %v", job, err)
	}
	clock.Add(1)
	awaitQueue(t, func() bool { rec, _ := s.GetEmail(ctx, id); return rec.Status == models.EmailStatusDelivered })
	rows, _ := s.GetDeliveriesForEmail(ctx, id)
	if len(rows) != 1 || rows[0].Attempts != 2 || rows[0].ResponseBody != "resumed" {
		t.Fatalf("deliveries: %+v", rows)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if count, err := s.Cleanup(ctx, 1); err != nil || count != 1 {
		t.Fatalf("retention after completion: %d, %v", count, err)
	}
}

func TestQueueCrashRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "crash.db")
	s, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id := saveQueueEmail(t, s)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	d := NewDispatcher(models.RetryConfig{Timeout: time.Second}, "test")
	if err := d.Enqueue(ctx, s, id, &models.ParsedEmail{}, []models.Rule{{Name: "crash", Webhook: models.WebhookConfig{URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimDelivery(ctx, time.Now().Add(time.Second))
	if err != nil || job == nil {
		t.Fatalf("claim: %+v, %v", job, err)
	}
	// Simulate process death after claiming, before saving a response.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ := s.GetDeliveriesForEmail(ctx, id)
	if rows[0].Status != "in_progress" {
		t.Fatalf("status=%s", rows[0].Status)
	}
	d = NewDispatcher(models.RetryConfig{}, "test")
	if err := d.StartQueue(s, 1, nil); err != nil {
		t.Fatal(err)
	}
	defer stopQueue(t, d)
	awaitQueue(t, func() bool { rec, _ := s.GetEmail(ctx, id); return rec.Status == models.EmailStatusDelivered })
	rows, _ = s.GetDeliveriesForEmail(ctx, id)
	if rows[0].Attempts != 1 {
		t.Fatalf("attempts=%d", rows[0].Attempts)
	}
}

func TestQueueShutdownAndConcurrency(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(filepath.Join(t.TempDir(), "shutdown.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		select {
		case <-release:
			w.WriteHeader(204)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	d := NewDispatcher(models.RetryConfig{Timeout: time.Minute}, "test")
	id := saveQueueEmail(t, s)
	rules := make([]models.Rule, 5)
	for i := range rules {
		rules[i] = models.Rule{Name: "bounded", Webhook: models.WebhookConfig{URL: server.URL}}
	}
	if err := d.Enqueue(ctx, s, id, &models.ParsedEmail{}, rules); err != nil {
		t.Fatal(err)
	}
	if err := d.StartQueue(s, 2, nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("workers did not start")
		}
	}
	d.StopQueue()
	shutdownCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := d.Shutdown(shutdownCtx); err != context.DeadlineExceeded {
		t.Fatalf("shutdown=%v", err)
	}
	select {
	case <-entered:
		t.Fatal("exceeded worker limit")
	default:
	}
	rows, _ := s.GetDeliveriesForEmail(ctx, id)
	for _, row := range rows {
		if row.Status != "pending" || row.Attempts != 0 {
			t.Fatalf("lost pending work: %+v", row)
		}
	}
}

func TestQueueGracefulDrain(t *testing.T) {
	s, err := storage.Open(filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { close(entered); <-release; w.WriteHeader(204) }))
	defer server.Close()
	d := NewDispatcher(models.RetryConfig{Timeout: time.Second}, "test")
	id := saveQueueEmail(t, s)
	if err := d.Enqueue(context.Background(), s, id, &models.ParsedEmail{}, []models.Rule{{Name: "drain", Webhook: models.WebhookConfig{URL: server.URL}}}); err != nil {
		t.Fatal(err)
	}
	if err := d.StartQueue(s, 1, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not start")
	}
	d.StopQueue()
	close(release)
	stopQueue(t, d)
	rec, _ := s.GetEmail(context.Background(), id)
	if rec.Status != models.EmailStatusDelivered {
		t.Fatalf("status=%s", rec.Status)
	}
}

func TestQueueRetryPolicies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		code         int
		timeout      bool
		override     bool
		wantAttempts int
		wantStatus   string
	}{
		{"server error retries", 503, false, false, 2, "failed"},
		{"client error final", 400, false, false, 1, "rejected"},
		{"override disables retries", 503, false, true, 1, "failed"},
		{"timeout retry disabled", 0, true, true, 1, "failed"},
		{"timeout retry enabled", 0, true, false, 2, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := storage.Open(filepath.Join(t.TempDir(), "policy.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.timeout {
					select {
					case <-r.Context().Done():
					case <-time.After(100 * time.Millisecond):
					}
					return
				}
				w.WriteHeader(tc.code)
			}))
			defer server.Close()
			d := NewDispatcher(models.RetryConfig{MaxRetries: 1, InitialWait: time.Millisecond, MaxWait: time.Millisecond, Timeout: 20 * time.Millisecond, RetryOnTimeout: true}, "test")
			wh := models.WebhookConfig{URL: server.URL}
			if tc.override {
				if tc.timeout {
					no := false
					wh.RetryOnTimeout = &no
				} else {
					zero := 0
					wh.MaxRetries = &zero
				}
			}
			id := saveQueueEmail(t, s)
			if err := d.Enqueue(context.Background(), s, id, &models.ParsedEmail{}, []models.Rule{{Name: tc.name, Webhook: wh}}); err != nil {
				t.Fatal(err)
			}
			if err := d.StartQueue(s, 1, nil); err != nil {
				t.Fatal(err)
			}
			defer stopQueue(t, d)
			awaitQueue(t, func() bool {
				rec, _ := s.GetEmail(context.Background(), id)
				return rec.Status == models.EmailStatusFailed
			})
			rows, _ := s.GetDeliveriesForEmail(context.Background(), id)
			if rows[0].Attempts != tc.wantAttempts || rows[0].Status != tc.wantStatus {
				t.Fatalf("delivery=%+v", rows[0])
			}
		})
	}
}

func TestQueueBackoffOverrides(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(filepath.Join(t.TempDir(), "backoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	now := time.Now().UTC()
	d := NewDispatcher(models.RetryConfig{}, "test")
	d.now = func() time.Time { return now }
	retries, initial, maximum, timeout := 3, 2, 3, 1
	rule := models.Rule{Name: "overrides", Webhook: models.WebhookConfig{URL: server.URL, MaxRetries: &retries, InitialWait: &initial, MaxWait: &maximum, Timeout: &timeout}}
	id := saveQueueEmail(t, s)
	if err := d.Enqueue(ctx, s, id, &models.ParsedEmail{}, []models.Rule{rule}); err != nil {
		t.Fatal(err)
	}
	q := &deliveryQueue{store: s}
	for _, delay := range []time.Duration{2 * time.Second, 3 * time.Second, 3 * time.Second, 0} {
		job, err := s.ClaimDelivery(ctx, now)
		if err != nil || job == nil {
			t.Fatalf("claim=%+v,%v", job, err)
		}
		d.runQueued(ctx, q, job)
		if delay == 0 {
			break
		}
		now = now.Add(delay - time.Nanosecond)
		early, err := s.ClaimDelivery(ctx, now)
		if err != nil || early != nil {
			t.Fatalf("early claim=%+v,%v", early, err)
		}
		now = now.Add(time.Nanosecond)
	}
	rows, _ := s.GetDeliveriesForEmail(ctx, id)
	if rows[0].Attempts != 4 || rows[0].Status != "failed" {
		t.Fatalf("delivery=%+v", rows[0])
	}
}

func TestQueueStartFailures(t *testing.T) {
	d := NewDispatcher(models.RetryConfig{}, "test")
	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := storage.Open(filepath.Join(t.TempDir(), "start.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := d.StartQueue(s, 0, nil); err == nil {
		t.Fatal("accepted zero workers")
	}
	if err := d.StartQueue(s, 1, nil); err != nil {
		t.Fatal(err)
	}
	defer stopQueue(t, d)
	if err := d.StartQueue(s, 1, nil); err == nil {
		t.Fatal("started second queue")
	}
	closed, err := storage.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	if err := NewDispatcher(models.RetryConfig{}, "test").StartQueue(closed, 1, nil); err == nil {
		t.Fatal("started without recovering database")
	}
}

func TestQueueTerminalErrors(t *testing.T) {
	for _, tc := range []struct {
		name, template, snapshot, wantStatus, wantError string
		private                                         bool
	}{
		{name: "malformed snapshot", snapshot: "{invalid", wantStatus: "error", wantError: "invalid character"},
		{name: "invalid template", template: "{{", wantStatus: "error", wantError: "parsing template"},
		{name: "template execution", template: `{{join "," "bad"}}`, wantStatus: "error", wantError: "executing template"},
		{name: "private target", private: true, wantStatus: "rejected", wantError: "private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := storage.Open(filepath.Join(t.TempDir(), "error.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := saveQueueEmail(t, s)
			d := NewDispatcher(models.RetryConfig{MaxRetries: 3, Timeout: time.Second}, "test")
			if tc.private {
				d.SetAllowPrivateNetworks(false)
			}
			now := time.Now().Add(time.Second)
			if tc.snapshot != "" {
				err = s.EnqueueDeliveries(context.Background(), id, []storage.QueuedDelivery{{RuleName: tc.name, Snapshot: []byte(tc.snapshot)}}, time.Now())
			} else {
				err = d.Enqueue(context.Background(), s, id, &models.ParsedEmail{}, []models.Rule{{Name: tc.name, Webhook: models.WebhookConfig{URL: "http://127.0.0.1:1", PayloadTemplate: tc.template}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			job, err := s.ClaimDelivery(context.Background(), now)
			if err != nil || job == nil {
				t.Fatalf("claim=%+v,%v", job, err)
			}
			var notified bool
			d.runQueued(context.Background(), &deliveryQueue{store: s, notify: func() { notified = true }}, job)
			rows, err := s.GetDeliveriesForEmail(context.Background(), id)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%+v,%v", rows, err)
			}
			if rows[0].Status != tc.wantStatus || !strings.Contains(rows[0].ErrorMessage, tc.wantError) {
				t.Fatalf("delivery=%+v", rows[0])
			}
			if !notified {
				t.Fatal("final result did not notify UI")
			}
			if job, err := s.ClaimDelivery(context.Background(), now.Add(24*time.Hour)); err != nil || job != nil {
				t.Fatalf("terminal error retried: %+v,%v", job, err)
			}
		})
	}
}

func TestQueueShutdownDuringClaim(t *testing.T) {
	for _, failRelease := range []bool{false, true} {
		t.Run(fmt.Sprint(failRelease), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "claim.db")
			s, err := storage.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			id := saveQueueEmail(t, s)
			d := NewDispatcher(models.RetryConfig{}, "test")
			if err := d.Enqueue(context.Background(), s, id, &models.ParsedEmail{}, []models.Rule{{Name: "claim", Webhook: models.WebhookConfig{URL: "http://127.0.0.1:1"}}}); err != nil {
				t.Fatal(err)
			}
			if failRelease {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				if _, err := db.Exec(`CREATE TRIGGER fail_release BEFORE UPDATE ON deliveries WHEN NEW.status='pending' BEGIN SELECT RAISE(ABORT,'release failed'); END`); err != nil {
					t.Fatal(err)
				}
			}
			q := &deliveryQueue{store: s, stop: make(chan struct{})}
			var once sync.Once
			// Stop after the worker's initial stop check, just before its database claim.
			d.now = func() time.Time { once.Do(func() { close(q.stop) }); return time.Now().UTC() }
			d.work(context.Background(), q)
			rows, err := s.GetDeliveriesForEmail(context.Background(), id)
			want := "pending"
			if failRelease {
				want = "in_progress"
			}
			if err != nil || len(rows) != 1 || rows[0].Status != want || rows[0].Attempts != 0 {
				t.Fatalf("delivery lost on shutdown: %+v,%v", rows, err)
			}
		})
	}
}

func TestQueueWorkerStorageFailures(t *testing.T) {
	path := filepath.Join(t.TempDir(), "write.db")
	s, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := saveQueueEmail(t, s)
	d := NewDispatcher(models.RetryConfig{}, "test")
	if err := d.Enqueue(context.Background(), s, id, &models.ParsedEmail{}, []models.Rule{{Name: "write", Webhook: models.WebhookConfig{PayloadTemplate: "{{"}}}); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimDelivery(context.Background(), time.Now().Add(time.Second))
	if err != nil || job == nil {
		t.Fatalf("claim=%+v,%v", job, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_save BEFORE UPDATE ON deliveries BEGIN SELECT RAISE(ABORT,'save failed'); END`); err != nil {
		t.Fatal(err)
	}
	notified := false
	d.runQueued(context.Background(), &deliveryQueue{store: s, notify: func() { notified = true }}, job)
	rows, err := s.GetDeliveriesForEmail(context.Background(), id)
	if err != nil || rows[0].Status != "in_progress" || notified {
		t.Fatalf("failed save lost work: %+v,%v; notified=%v", rows, err, notified)
	}
	s.Close()
	q := &deliveryQueue{store: s, stop: make(chan struct{})}
	d.now = func() time.Time { close(q.stop); return time.Now() }
	d.work(context.Background(), q)
}
