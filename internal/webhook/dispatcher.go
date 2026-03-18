package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"text/template"
	"time"

	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
)

// Dispatcher sends webhook requests for matched rules.
type Dispatcher struct {
	client    *http.Client
	retryMu   sync.RWMutex
	retry     models.RetryConfig
	userAgent string
}

// NewDispatcher creates a new webhook dispatcher.
func NewDispatcher(retry models.RetryConfig, version string) *Dispatcher {
	return &Dispatcher{
		client:    &http.Client{Timeout: 30 * time.Second},
		retry:     retry,
		userAgent: "mailrelay/" + version,
	}
}

// Dispatch fires webhooks for all matched rules concurrently and returns results.
func (d *Dispatcher) Dispatch(ctx context.Context, email *models.ParsedEmail, rules []models.Rule) []models.DeliveryResult {
	results := make([]models.DeliveryResult, len(rules))
	var wg sync.WaitGroup
	for i, rule := range rules {
		wg.Add(1)
		go func(idx int, r models.Rule) {
			defer wg.Done()
			results[idx] = d.dispatchOne(ctx, email, r)
		}(i, rule)
	}
	wg.Wait()
	return results
}

func (d *Dispatcher) dispatchOne(ctx context.Context, email *models.ParsedEmail, rule models.Rule) models.DeliveryResult {
	result := models.DeliveryResult{RuleName: rule.Name}
	retry := d.getRetryConfig()

	payload, err := buildPayload(email, rule.Webhook)
	if err != nil {
		slog.Error("failed to build webhook payload",
			"rule", rule.Name, "error", err)
		metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "error").Inc()
		result.Status = "error"
		result.Error = err.Error()
		return result
	}

	var lastErr error
	for attempt := 0; attempt <= retry.MaxRetries; attempt++ {
		result.Attempts = attempt + 1

		if attempt > 0 {
			metrics.WebhookRetriesTotal.WithLabelValues(rule.Name).Inc()
			wait := backoff(retry, attempt)
			slog.Info("retrying webhook",
				"rule", rule.Name, "attempt", attempt, "wait", wait)
			select {
			case <-ctx.Done():
				slog.Warn("webhook dispatch cancelled",
					"rule", rule.Name, "error", ctx.Err())
				metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "cancelled").Inc()
				result.Status = "cancelled"
				result.Error = ctx.Err().Error()
				return result
			case <-time.After(wait):
			}
		}

		start := time.Now()
		metrics.WebhookInFlight.Inc()
		var respBody string
		respBody, lastErr = d.send(ctx, rule, payload)
		metrics.WebhookInFlight.Dec()
		metrics.WebhookDurationSeconds.WithLabelValues(rule.Name).Observe(time.Since(start).Seconds())

		if lastErr == nil {
			slog.Info("webhook delivered", "rule", rule.Name)
			metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "success").Inc()
			result.Status = "success"
			result.ResponseBody = respBody
			return result
		}

		if whErr, ok := lastErr.(*webhookError); ok && whErr.statusCode >= 400 && whErr.statusCode < 500 {
			slog.Error("webhook rejected (not retryable)",
				"rule", rule.Name, "status", whErr.statusCode, "error", lastErr)
			metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "rejected").Inc()
			result.Status = "rejected"
			result.StatusCode = whErr.statusCode
			result.Error = lastErr.Error()
			result.ResponseBody = whErr.responseBody
			return result
		}

		slog.Warn("webhook attempt failed",
			"rule", rule.Name, "attempt", attempt, "error", lastErr)
	}

	slog.Error("webhook delivery failed after retries",
		"rule", rule.Name, "max_retries", retry.MaxRetries, "error", lastErr)
	metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "failure").Inc()
	result.Status = "failed"
	if lastErr != nil {
		result.Error = lastErr.Error()
		if whErr, ok := lastErr.(*webhookError); ok {
			result.StatusCode = whErr.statusCode
			result.ResponseBody = whErr.responseBody
		}
	}
	return result
}

func (d *Dispatcher) send(ctx context.Context, rule models.Rule, payload []byte) (string, error) {
	method := rule.Webhook.Method
	if method == "" {
		method = http.MethodPost
	}

	req, err := http.NewRequestWithContext(ctx, method, rule.Webhook.URL, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("User-Agent", d.userAgent)

	// Set default content type if not specified in headers.
	hasContentType := false
	for k, v := range rule.Webhook.Headers {
		req.Header.Set(k, v)
		if http.CanonicalHeaderKey(k) == "Content-Type" {
			hasContentType = true
		}
	}
	if !hasContentType {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return string(body), nil
	}

	return "", &webhookError{statusCode: resp.StatusCode, responseBody: string(body)}
}

func backoff(retry models.RetryConfig, attempt int) time.Duration {
	wait := time.Duration(float64(retry.InitialWait) * math.Pow(2, float64(attempt-1)))
	return min(wait, retry.MaxWait)
}

// SetRetryConfig hot-reloads the retry configuration.
func (d *Dispatcher) SetRetryConfig(cfg models.RetryConfig) {
	d.retryMu.Lock()
	d.retry = cfg
	d.retryMu.Unlock()
}

func (d *Dispatcher) getRetryConfig() models.RetryConfig {
	d.retryMu.RLock()
	defer d.retryMu.RUnlock()
	return d.retry
}

// rawJSON is a pre-encoded JSON value that renders as-is in templates,
// so {{.TextBody}} produces a valid JSON string literal (quoted + escaped)
// rather than a raw Go string with unescaped newlines.
type rawJSON []byte

func (r rawJSON) String() string {
	if r == nil {
		return "null"
	}
	return string(r)
}

// emailTemplateData converts a ParsedEmail to a map keyed by Go field names
// where every value is already JSON-encoded. This makes template variables
// safe to embed directly in a JSON payload without further escaping.
func emailTemplateData(email *models.ParsedEmail) (map[string]rawJSON, error) {
	b, err := json.Marshal(email)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	// Re-map snake_case JSON keys to Go field names for ergonomic template access.
	mapping := map[string]string{
		"From":         "from",
		"To":           "to",
		"CC":           "cc",
		"Subject":      "subject",
		"TextBody":     "text_body",
		"HTMLBody":     "html_body",
		"Headers":      "headers",
		"Attachments":  "attachments",
		"AuthResult":   "auth_result",
		"EnvelopeFrom": "envelope_from",
		"EnvelopeTo":   "envelope_to",
	}
	data := make(map[string]rawJSON, len(mapping))
	for goName, jsonKey := range mapping {
		if v, ok := raw[jsonKey]; ok {
			data[goName] = rawJSON(v)
		} else {
			data[goName] = rawJSON("null")
		}
	}
	return data, nil
}

func buildPayload(email *models.ParsedEmail, wh models.WebhookConfig) ([]byte, error) {
	if wh.PayloadTemplate != "" {
		data, err := emailTemplateData(email)
		if err != nil {
			return nil, fmt.Errorf("preparing template data: %w", err)
		}
		tmpl, err := template.New("payload").Parse(wh.PayloadTemplate)
		if err != nil {
			return nil, fmt.Errorf("parsing template: %w", err)
		}
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, data); err != nil {
			return nil, fmt.Errorf("executing template: %w", err)
		}
		return buf.Bytes(), nil
	}

	return json.Marshal(email)
}

type webhookError struct {
	statusCode   int
	responseBody string
}

func (e *webhookError) Error() string {
	return fmt.Sprintf("webhook returned status %d", e.statusCode)
}
