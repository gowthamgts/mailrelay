package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/gowthamgts/mailrelay/internal/models"
)

// QueuedDelivery holds a durable request snapshot and the completed attempt count.
type QueuedDelivery struct {
	ID            string
	EmailID       string
	RuleName      string
	Snapshot      []byte
	Attempts      int
	NextAttemptAt time.Time
}

// EnqueueDeliveries atomically publishes a batch and marks the email unfinished.
// The most recent batch determines the final email status after all work finishes.
func (s *Store) EnqueueDeliveries(ctx context.Context, emailID string, jobs []QueuedDelivery, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	batch := uuid.NewString()
	for _, job := range jobs {
		id := uuid.NewString()
		if _, err = tx.ExecContext(ctx, `INSERT INTO deliveries (id,email_id,rule_name,status,created_at,updated_at) VALUES (?,?,?,'pending',?,?)`, id, emailID, job.RuleName, now, now); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO delivery_queue (delivery_id,batch_id,email_id,snapshot,next_attempt_at) VALUES (?,?,?,?,?)`, id, batch, emailID, string(job.Snapshot), now.UnixNano()); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE emails SET status='received',delivery_batch=? WHERE id=?`, batch, emailID); err != nil {
		return err
	}
	return tx.Commit()
}

// RecoverDeliveries is called once, before starting workers. A request interrupted
// by a crash has an unknown outcome, so it is sent again (at-least-once delivery).
func (s *Store) RecoverDeliveries(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deliveries SET status='pending' WHERE status='in_progress' AND id IN (SELECT delivery_id FROM delivery_queue)`)
	return err
}

// ClaimDelivery serializes selection and claiming so workers cannot send the same row.
func (s *Store) ClaimDelivery(ctx context.Context, now time.Time) (*QueuedDelivery, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var job QueuedDelivery
	var due int64
	err = tx.QueryRowContext(ctx, `SELECT d.id,q.email_id,d.rule_name,q.snapshot,d.attempts,q.next_attempt_at FROM delivery_queue q JOIN deliveries d ON d.id=q.delivery_id JOIN emails e ON e.id=q.email_id WHERE d.status='pending' AND q.next_attempt_at<=? ORDER BY q.next_attempt_at LIMIT 1`, now.UnixNano()).Scan(&job.ID, &job.EmailID, &job.RuleName, &job.Snapshot, &job.Attempts, &due)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.NextAttemptAt = time.Unix(0, due)
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET status='in_progress',updated_at=? WHERE id=?`, now, job.ID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

// FinishDelivery saves each attempt and the next due time in one transaction.
// A zero next time marks a final result. Cancellation leaves the attempt count
// unchanged, since shutdown should not exhaust the receiver's retry budget.
func (s *Store) FinishDelivery(ctx context.Context, job *QueuedDelivery, result models.DeliveryResult, next, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE deliveries SET status=?,status_code=?,error_message=?,response_body=?,attempts=?,updated_at=? WHERE id=?`, result.Status, result.StatusCode, result.Error, result.ResponseBody, result.Attempts, now, job.ID); err != nil {
		return err
	}
	if !next.IsZero() {
		if _, err = tx.ExecContext(ctx, `UPDATE delivery_queue SET next_attempt_at=? WHERE delivery_id=?`, next.UnixNano(), job.ID); err != nil {
			return err
		}
	} else {
		// Keep batch membership for aggregate status, but release the request snapshot.
		// The maximum timestamp keeps finished rows out of the due-time index scan.
		if _, err = tx.ExecContext(ctx, `UPDATE delivery_queue SET snapshot='',next_attempt_at=9223372036854775807 WHERE delivery_id=?`, job.ID); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE emails SET status=(
 SELECT CASE WHEN SUM(d.status='success')=COUNT(*) THEN 'delivered'
 WHEN SUM(d.status='success')>0 THEN 'partial_failure' ELSE 'failed' END
 FROM deliveries d JOIN delivery_queue q ON q.delivery_id=d.id
 WHERE q.email_id=emails.id AND q.batch_id=emails.delivery_batch)
 WHERE id=? AND NOT EXISTS (SELECT 1 FROM deliveries d JOIN delivery_queue q ON q.delivery_id=d.id WHERE q.email_id=emails.id AND d.status IN ('pending','in_progress'))`, job.EmailID)
	if err != nil {
		return err
	}
	return tx.Commit()
}
