package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/storage"
)

type deliverySnapshot struct {
	Rule         models.Rule
	Payload      []byte
	Retry        models.RetryConfig
	PayloadError string
}

type deliveryQueue struct {
	store  *storage.Store
	stop   chan struct{}
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
	notify func()
}

// StartQueue resumes persisted work using a fixed number of workers. Only one
// dispatcher process may own a database. Retry settings are captured at enqueue.
func (d *Dispatcher) StartQueue(store *storage.Store, workers int, notify func()) error {
	if d.queue != nil {
		return fmt.Errorf("delivery queue already started")
	}
	if workers < 1 {
		return fmt.Errorf("workers must be positive")
	}
	if err := store.RecoverDeliveries(context.Background()); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &deliveryQueue{store: store, stop: make(chan struct{}), cancel: cancel, done: make(chan struct{}), notify: notify}
	d.queue = q
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() { defer wg.Done(); d.work(ctx, q) }()
	}
	go func() { wg.Wait(); close(q.done) }()
	return nil
}

// StopQueue stops workers from claiming new work. In-flight requests continue.
func (d *Dispatcher) StopQueue() {
	if d.queue != nil {
		d.queue.once.Do(func() { close(d.queue.stop) })
	}
}

// Shutdown stops claiming work, waits for active requests until ctx expires,
// then cancels requests and waits for their state to be safely persisted.
func (d *Dispatcher) Shutdown(ctx context.Context) error {
	q := d.queue
	if q == nil {
		return nil
	}
	d.StopQueue()
	defer q.cancel()
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		q.cancel()
		<-q.done
		return ctx.Err()
	}
}

// Enqueue persists a complete batch without waiting for network requests.
func (d *Dispatcher) Enqueue(ctx context.Context, store *storage.Store, emailID string, email *models.ParsedEmail, rules []models.Rule) error {
	jobs := make([]storage.QueuedDelivery, 0, len(rules))
	global := d.getRetryConfig()
	for _, rule := range rules {
		wh := rule.Webhook
		retry := models.RetryConfig{
			MaxRetries:     wh.EffectiveMaxRetries(global.MaxRetries),
			InitialWait:    wh.EffectiveInitialWait(global.InitialWait),
			MaxWait:        wh.EffectiveMaxWait(global.MaxWait),
			Timeout:        wh.EffectiveTimeout(global.Timeout),
			RetryOnTimeout: wh.EffectiveRetryOnTimeout(global.RetryOnTimeout),
		}
		payload, err := buildPayload(email, wh)
		snap := deliverySnapshot{Rule: rule, Payload: payload, Retry: retry}
		if err != nil {
			snap.PayloadError = err.Error()
		}
		data, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		jobs = append(jobs, storage.QueuedDelivery{RuleName: rule.Name, Snapshot: data})
	}
	return store.EnqueueDeliveries(ctx, emailID, jobs, d.now().UTC())
}

func (d *Dispatcher) work(ctx context.Context, q *deliveryQueue) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		job, err := q.store.ClaimDelivery(ctx, d.now().UTC())
		if err != nil && ctx.Err() == nil {
			slog.Error("claiming webhook delivery", "error", err)
		}
		if job != nil {
			// Recheck after the database call so shutdown cannot start another request.
			select {
			case <-q.stop:
				result := models.DeliveryResult{Status: "pending", Attempts: job.Attempts}
				if err := q.store.FinishDelivery(context.Background(), job, result, d.now().UTC(), d.now().UTC()); err != nil {
					slog.Error("releasing delivery", "error", err)
				}
				return
			default:
			}
			d.runQueued(ctx, q, job)
			continue
		}
		select {
		case <-q.stop:
			return
		case <-ticker.C:
		}
	}
}

func (d *Dispatcher) runQueued(ctx context.Context, q *deliveryQueue, job *storage.QueuedDelivery) {
	var snap deliverySnapshot
	result := models.DeliveryResult{RuleName: job.RuleName, Attempts: job.Attempts, Status: "error"}
	var next time.Time
	if err := json.Unmarshal(job.Snapshot, &snap); err != nil {
		result.Error = err.Error()
	} else if snap.PayloadError != "" {
		result.Error = snap.PayloadError
	} else {
		result.Attempts++
		if job.Attempts > 0 {
			metrics.WebhookRetriesTotal.WithLabelValues(job.RuleName).Inc()
		}
		start := time.Now()
		metrics.WebhookInFlight.Inc()
		body, err := d.send(ctx, snap.Rule, snap.Payload, snap.Retry.Timeout, job.ID)
		metrics.WebhookInFlight.Dec()
		metrics.WebhookDurationSeconds.WithLabelValues(job.RuleName).Observe(time.Since(start).Seconds())
		result.Status = "success"
		result.ResponseBody = body
		if err != nil {
			result.Status = "failed"
			result.Error = err.Error()
			var whErr *webhookError
			retryable := true
			if errors.As(err, &whErr) {
				result.StatusCode = whErr.statusCode
				result.ResponseBody = whErr.responseBody
				if whErr.statusCode >= 400 && whErr.statusCode < 500 {
					result.Status = "rejected"
					retryable = false
				}
			}
			if errors.Is(err, ErrPrivateAddress) {
				result.Status = "rejected"
				retryable = false
			}
			if isTimeoutError(err) && !snap.Retry.RetryOnTimeout {
				retryable = false
			}
			if ctx.Err() != nil {
				result.Status = "pending"
				result.Attempts = job.Attempts
				next = d.now().UTC()
			} else if retryable && result.Attempts <= snap.Retry.MaxRetries {
				result.Status = "pending"
				next = d.now().UTC().Add(backoff(snap.Retry, result.Attempts))
			}
		}
	}
	slog.Info("webhook attempt completed", "rule", job.RuleName, "attempts", result.Attempts, "status", result.Status, "next_attempt_at", next, "error", result.Error)
	if err := q.store.FinishDelivery(context.Background(), job, result, next, d.now().UTC()); err != nil {
		// Leave the row in progress on a storage failure; startup recovery retries it.
		slog.Error("saving webhook attempt", "delivery", job.ID, "error", err)
		return
	}
	if result.Status != "pending" {
		label := result.Status
		if label == "failed" {
			label = "failure"
		}
		metrics.WebhookDispatchesTotal.WithLabelValues(job.RuleName, label).Inc()
	}
	if q.notify != nil {
		q.notify()
	}
}
