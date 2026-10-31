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
