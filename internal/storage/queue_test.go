package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestQueueMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Schema from before persistent deliveries and the additive column migrations.
	_, err = db.Exec(`CREATE TABLE emails (id TEXT PRIMARY KEY,received_at DATETIME NOT NULL,envelope_from TEXT NOT NULL,envelope_to TEXT NOT NULL,subject TEXT NOT NULL DEFAULT '',status TEXT NOT NULL DEFAULT 'received',parsed_email TEXT NOT NULL,auth_result TEXT);
 CREATE TABLE deliveries (id TEXT PRIMARY KEY,email_id TEXT NOT NULL REFERENCES emails(id) ON DELETE CASCADE,rule_name TEXT NOT NULL,status TEXT NOT NULL,status_code INTEGER NOT NULL DEFAULT 0,error_message TEXT NOT NULL DEFAULT '',attempts INTEGER NOT NULL DEFAULT 0,created_at DATETIME NOT NULL,updated_at DATETIME NOT NULL);
 INSERT INTO emails (id,received_at,envelope_from,envelope_to,parsed_email) VALUES ('legacy',CURRENT_TIMESTAMP,'sender@example.com','[]','{}');`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for range 2 {
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.GetEmail(context.Background(), "legacy"); err != nil {
			t.Fatal(err)
		}
		if err = s.EnqueueDeliveries(context.Background(), "legacy", []QueuedDelivery{{RuleName: "migrated", Snapshot: []byte(`{}`)}}, time.Now()); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
}

func TestQueueBatchStatusAndDeletion(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	rec := sampleEmail("batch")
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	enqueue := func(n int) {
		t.Helper()
		jobs := make([]QueuedDelivery, n)
		for i := range jobs {
			jobs[i] = QueuedDelivery{RuleName: "batch", Snapshot: []byte(`{}`)}
		}
		if err := s.EnqueueDeliveries(ctx, rec.ID, jobs, now); err != nil {
			t.Fatal(err)
		}
	}
	claim := func() *QueuedDelivery {
		t.Helper()
		job, err := s.ClaimDelivery(ctx, now)
		if err != nil || job == nil {
			t.Fatalf("claim=%+v,%v", job, err)
		}
		return job
	}
	finish := func(job *QueuedDelivery, status string) {
		t.Helper()
		if err := s.FinishDelivery(ctx, job, models.DeliveryResult{Status: status, Attempts: 1}, time.Time{}, now); err != nil {
			t.Fatal(err)
		}
	}
	status := func(want models.EmailStatus) {
		t.Helper()
		rec, err := s.GetEmail(ctx, rec.ID)
		if err != nil || rec.Status != want {
			t.Fatalf("email=%+v,%v; want %s", rec, err, want)
		}
	}
	enqueue(2)
	finish(claim(), "success")
	status(models.EmailStatusReceived)
	finish(claim(), "failed")
	status(models.EmailStatusPartialFailure)
	// A replay's result supersedes previous batches while retaining their records.
	enqueue(1)
	finish(claim(), "success")
	status(models.EmailStatusDelivered)
	rows, err := s.GetDeliveriesForEmail(ctx, rec.ID)
	if err != nil || len(rows) != 3 {
		t.Fatalf("history=%+v,%v", rows, err)
	}
	enqueue(1)
	if err := s.UpdateEmailStatus(ctx, rec.ID, models.EmailStatusDropped); err != nil {
		t.Fatal(err)
	}
	status(models.EmailStatusReceived)
	if err := s.DeleteEmail(ctx, rec.ID); err != nil {
		t.Fatal(err)
	}
	job, err := s.ClaimDelivery(ctx, now)
	if err != nil || job != nil {
		t.Fatalf("deleted work claimed: %+v,%v", job, err)
	}
	var remaining int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM delivery_queue`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("orphan jobs=%d,%v", remaining, err)
	}
	rows, err = s.GetDeliveriesForEmail(ctx, rec.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("orphan deliveries=%+v,%v", rows, err)
	}
}

// Real SQLite failures verify that publishing and finishing deliveries are atomic,
// including errors after earlier statements have already modified a transaction.
func TestQueueTransactionRollback(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		enqueue       bool
		pending       bool
	}{
		{name: "enqueue delivery", trigger: "BEFORE INSERT ON deliveries", enqueue: true},
		{name: "enqueue snapshot", trigger: "BEFORE INSERT ON delivery_queue", enqueue: true},
		{name: "enqueue email status", trigger: "BEFORE UPDATE ON emails", enqueue: true},
		{name: "finish result", trigger: "BEFORE UPDATE ON deliveries"},
		{name: "schedule retry", trigger: "BEFORE UPDATE ON delivery_queue", pending: true},
		{name: "finish snapshot", trigger: "BEFORE UPDATE ON delivery_queue"},
		{name: "finish email status", trigger: "BEFORE UPDATE ON emails"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := openTestStore(t)
			rec := sampleEmail("rollback")
			rec.Status = models.EmailStatusFailed
			if err := s.SaveEmail(ctx, rec); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			enqueue := func() error {
				return s.EnqueueDeliveries(ctx, rec.ID, []QueuedDelivery{{RuleName: "rollback", Snapshot: []byte("request")}}, now)
			}
			var job *QueuedDelivery
			if !tc.enqueue {
				if err := enqueue(); err != nil {
					t.Fatal(err)
				}
				var err error
				job, err = s.ClaimDelivery(ctx, now)
				if err != nil || job == nil {
					t.Fatalf("claim=%+v,%v", job, err)
				}
			}
			if _, err := s.db.Exec("CREATE TRIGGER fail_write " + tc.trigger + " BEGIN SELECT RAISE(ABORT, 'injected write failure'); END"); err != nil {
				t.Fatal(err)
			}
			if tc.enqueue {
				if err := enqueue(); err == nil {
					t.Fatal("expected enqueue failure")
				}
				rows, err := s.GetDeliveriesForEmail(ctx, rec.ID)
				if err != nil || len(rows) != 0 {
					t.Fatalf("partial deliveries committed: %+v,%v", rows, err)
				}
				var count int
				if err := s.db.QueryRow(`SELECT COUNT(*) FROM delivery_queue`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("partial snapshots committed: %d,%v", count, err)
				}
				email, err := s.GetEmail(ctx, rec.ID)
				if err != nil || email.Status != models.EmailStatusFailed {
					t.Fatalf("email changed: %+v,%v", email, err)
				}
			} else {
				next := time.Time{}
				result := models.DeliveryResult{Status: "success", Attempts: 1, ResponseBody: "delivered"}
				if tc.pending {
					next = now.Add(time.Hour)
					result.Status = "pending"
				}
				if err := s.FinishDelivery(ctx, job, result, next, now); err == nil {
					t.Fatal("expected finish failure")
				}
				rows, err := s.GetDeliveriesForEmail(ctx, rec.ID)
				if err != nil || len(rows) != 1 || rows[0].Status != "in_progress" || rows[0].Attempts != 0 || rows[0].ResponseBody != "" {
					t.Fatalf("partial result committed: %+v,%v", rows, err)
				}
				var snapshot string
				var due int64
				if err := s.db.QueryRow(`SELECT snapshot,next_attempt_at FROM delivery_queue`).Scan(&snapshot, &due); err != nil || snapshot != "request" || due != now.UnixNano() {
					t.Fatalf("snapshot changed: %q,%d,%v", snapshot, due, err)
				}
			}
		})
	}
}

func TestQueueUnavailableDatabase(t *testing.T) {
	s := openTestStore(t)
	s.Close()
	ctx := context.Background()
	now := time.Now()
	if err := s.EnqueueDeliveries(ctx, "missing", nil, now); err == nil {
		t.Error("enqueue succeeded on closed database")
	}
	if _, err := s.ClaimDelivery(ctx, now); err == nil {
		t.Error("claim succeeded on closed database")
	}
	if err := s.FinishDelivery(ctx, &QueuedDelivery{}, models.DeliveryResult{}, time.Time{}, now); err == nil {
		t.Error("finish succeeded on closed database")
	}
	if err := s.RecoverDeliveries(ctx); err == nil {
		t.Error("recovery succeeded on closed database")
	}
	if _, err := s.CleanupExpired(ctx, 1); err == nil {
		t.Error("cleanup succeeded on closed database")
	}
}

func TestQueueClaimFailures(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"read", `DROP TABLE delivery_queue`},
		{"decode attempts", `UPDATE deliveries SET attempts='invalid'`},
		{"commit", `CREATE TABLE commit_parent(id INTEGER PRIMARY KEY);
 CREATE TABLE commit_child(parent_id INTEGER REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED);
 CREATE TRIGGER fail_commit AFTER UPDATE ON deliveries BEGIN INSERT INTO commit_child VALUES (1); END`},
		{"claim write", `CREATE TRIGGER fail_claim BEFORE UPDATE ON deliveries BEGIN SELECT RAISE(ABORT, 'claim failed'); END`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTestStore(t)
			ctx := context.Background()
			rec := sampleEmail("claim failure")
			if err := s.SaveEmail(ctx, rec); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := s.EnqueueDeliveries(ctx, rec.ID, []QueuedDelivery{{RuleName: "claim", Snapshot: []byte("request")}}, now); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			if job, err := s.ClaimDelivery(ctx, now); err == nil || job != nil {
				t.Fatalf("claim=%+v,%v", job, err)
			}
			rows, err := s.GetDeliveriesForEmail(ctx, rec.ID)
			if tc.name != "decode attempts" && (err != nil || rows[0].Status != "pending") {
				t.Fatalf("failed claim changed delivery: %+v,%v", rows, err)
			}
		})
	}
}
