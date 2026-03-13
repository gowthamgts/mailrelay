package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const namespace = "mailrelay"

// SMTP metrics.
var (
	SMTPConnectionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "smtp",
		Name:      "connections_total",
		Help:      "Total number of SMTP connections accepted.",
	})

	SMTPEmailsReceivedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "smtp",
		Name:      "emails_received_total",
		Help:      "Total number of emails successfully received.",
	})

	SMTPRecipientsRejectedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "smtp",
		Name:      "recipients_rejected_total",
		Help:      "Total number of recipients rejected by allowed_recipients filter.",
	})

	SMTPEmailSizeBytes = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "smtp",
		Name:      "email_size_bytes",
		Help:      "Size of received emails in bytes.",
		Buckets:   []float64{1024, 10240, 102400, 1048576, 5242880, 10485760, 26214400},
	})

	SMTPEmailProcessingDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "smtp",
		Name:      "email_processing_duration_seconds",
		Help:      "Time spent processing emails in the DATA handler (auth + parsing).",
		Buckets:   prometheus.DefBuckets,
	})

	SMTPEmailErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "smtp",
		Name:      "email_errors_total",
		Help:      "Total number of email processing errors by stage.",
	}, []string{"stage"})
)

// Auth metrics.
var (
	AuthChecksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "auth",
		Name:      "checks_total",
		Help:      "Total number of authentication checks performed.",
	}, []string{"check", "result"})

	AuthEnforcementFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "auth",
		Name:      "enforcement_failures_total",
		Help:      "Total number of emails rejected due to auth enforcement.",
	}, []string{"check"})
)

// Rule engine metrics.
var (
	RulesMatchedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "rules",
		Name:      "matched_total",
		Help:      "Total number of times each rule matched an email.",
	}, []string{"rule"})

	RulesEvaluatedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "rules",
		Name:      "evaluated_total",
		Help:      "Total number of emails evaluated against rules.",
	})

	RulesNoMatchTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "rules",
		Name:      "no_match_total",
		Help:      "Total number of emails that matched no rules.",
	})
)

// Webhook metrics.
var (
	WebhookDispatchesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "dispatches_total",
		Help:      "Total number of webhook dispatch outcomes.",
	}, []string{"rule", "status"})

	WebhookDurationSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "duration_seconds",
		Help:      "Duration of individual webhook HTTP requests in seconds.",
		Buckets:   prometheus.DefBuckets,
	}, []string{"rule"})

	WebhookRetriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "retries_total",
		Help:      "Total number of webhook retry attempts.",
	}, []string{"rule"})

	WebhookInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "webhook",
		Name:      "in_flight",
		Help:      "Number of webhook requests currently in flight.",
	})
)
