package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
)

// Dispatcher sends webhook requests for matched rules.
type Dispatcher struct {
	client       *http.Client
	retryMu      sync.RWMutex
	retry        models.RetryConfig
	userAgent    string
	allowPrivate atomic.Bool
}

// NewDispatcher creates a new webhook dispatcher. It can reach any address
// until SetAllowPrivateNetworks(false) is called; main applies the config.
func NewDispatcher(retry models.RetryConfig, version string) *Dispatcher {
	d := &Dispatcher{
		retry:     retry,
		userAgent: "mailrelay/" + version,
	}
	d.allowPrivate.Store(true)
	d.client = &http.Client{Transport: newTransport(&d.allowPrivate)}
	return d
}

// SetAllowPrivateNetworks controls whether webhooks (and the UI's Test button)
// may connect to loopback, private, link-local and other internal addresses.
func (d *Dispatcher) SetAllowPrivateNetworks(allow bool) {
	d.allowPrivate.Store(allow)
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
	globalRetry := d.getRetryConfig()

	// Resolve per-webhook overrides.
	wh := &rule.Webhook
	maxRetries := wh.EffectiveMaxRetries(globalRetry.MaxRetries)
	initialWait := wh.EffectiveInitialWait(globalRetry.InitialWait)
	maxWait := wh.EffectiveMaxWait(globalRetry.MaxWait)
	timeout := wh.EffectiveTimeout(globalRetry.Timeout)
	retryOnTimeout := wh.EffectiveRetryOnTimeout(globalRetry.RetryOnTimeout)

	localRetry := models.RetryConfig{
		MaxRetries:  maxRetries,
		InitialWait: initialWait,
		MaxWait:     maxWait,
	}

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
	for attempt := 0; attempt <= maxRetries; attempt++ {
		result.Attempts = attempt + 1

		if attempt > 0 {
			metrics.WebhookRetriesTotal.WithLabelValues(rule.Name).Inc()
			wait := backoff(localRetry, attempt)
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
		respBody, lastErr = d.send(ctx, rule, payload, timeout)
		metrics.WebhookInFlight.Dec()
		metrics.WebhookDurationSeconds.WithLabelValues(rule.Name).Observe(time.Since(start).Seconds())

		if lastErr == nil {
			slog.Info("webhook delivered", "rule", rule.Name)
			metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "success").Inc()
			result.Status = "success"
			result.ResponseBody = respBody
			return result
		}

		if errors.Is(lastErr, ErrPrivateAddress) {
			slog.Error("webhook blocked (private address)", "rule", rule.Name, "error", lastErr)
			metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "rejected").Inc()
			result.Status = "rejected"
			result.Error = lastErr.Error()
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

		if isTimeoutError(lastErr) && !retryOnTimeout {
			slog.Error("webhook timed out (retry_on_timeout disabled)",
				"rule", rule.Name, "error", lastErr)
			metrics.WebhookDispatchesTotal.WithLabelValues(rule.Name, "failure").Inc()
			result.Status = "failed"
			result.Error = lastErr.Error()
			return result
		}

		slog.Warn("webhook attempt failed",
			"rule", rule.Name, "attempt", attempt, "error", lastErr)
	}

	slog.Error("webhook delivery failed after retries",
		"rule", rule.Name, "max_retries", maxRetries, "error", lastErr)
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

func (d *Dispatcher) send(ctx context.Context, rule models.Rule, payload []byte, timeout time.Duration) (string, error) {
	method := rule.Webhook.Method
	if method == "" {
		method = http.MethodPost
	}

	reqCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		reqCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	req, err := http.NewRequestWithContext(reqCtx, method, rule.Webhook.URL, bytes.NewReader(payload))
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

// TemplateFuncs are the helper functions available in payload templates. They
// operate on (and return) pre-encoded JSON values, so their output can be
// embedded in a JSON payload just like a plain variable.
var TemplateFuncs = template.FuncMap{
	"first":   jsonFirst,
	"last":    jsonLast,
	"join":    jsonJoin,
	"default": jsonDefault,
}

// ParseTemplate parses a payload template with TemplateFuncs registered.
func ParseTemplate(text string) (*template.Template, error) {
	return template.New("payload").Funcs(TemplateFuncs).Parse(text)
}

// jsonArray decodes v as a JSON array; anything else yields nil.
func jsonArray(v rawJSON) []json.RawMessage {
	var arr []json.RawMessage
	if err := json.Unmarshal(v, &arr); err != nil {
		return nil
	}
	return arr
}

// jsonFirst returns the first element of a JSON array, or null if empty.
func jsonFirst(v rawJSON) rawJSON {
	if arr := jsonArray(v); len(arr) > 0 {
		return rawJSON(arr[0])
	}
	return rawJSON("null")
}

// jsonLast returns the last element of a JSON array, or null if empty.
func jsonLast(v rawJSON) rawJSON {
	if arr := jsonArray(v); len(arr) > 0 {
		return rawJSON(arr[len(arr)-1])
	}
	return rawJSON("null")
}

// jsonJoin joins the elements of a JSON array into a single JSON string.
// String elements are joined unquoted; other elements use their JSON text.
func jsonJoin(sep string, v rawJSON) (rawJSON, error) {
	arr := jsonArray(v)
	parts := make([]string, 0, len(arr))
	for _, el := range arr {
		var s string
		if err := json.Unmarshal(el, &s); err == nil {
			parts = append(parts, s)
		} else {
			parts = append(parts, string(el))
		}
	}
	return json.Marshal(strings.Join(parts, sep))
}

// jsonDefault returns v unless it is empty (null, "", [] or {}), in which case
// it returns def. def may be another template variable (already JSON) or a
// literal, which is JSON-encoded.
func jsonDefault(def any, v rawJSON) (rawJSON, error) {
	switch strings.TrimSpace(string(v)) {
	case "", "null", `""`, "[]", "{}":
	default:
		return v, nil
	}
	if r, ok := def.(rawJSON); ok {
		return r, nil
	}
	return json.Marshal(def)
}

func buildPayload(email *models.ParsedEmail, wh models.WebhookConfig) ([]byte, error) {
	if wh.PayloadTemplate != "" {
		data, err := emailTemplateData(email)
		if err != nil {
			return nil, fmt.Errorf("preparing template data: %w", err)
		}
		tmpl, err := ParseTemplate(wh.PayloadTemplate)
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

// isTimeoutError checks whether err is a timeout (context deadline or net timeout).
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// maxTestResponseBody caps how much of a test response is returned.
const maxTestResponseBody = 64 * 1024

// TestResult is the outcome of a TestConnectivity request.
type TestResult struct {
	StatusCode  int
	ContentType string
	Body        string
	Truncated   bool
}

// TestConnectivity sends a lightweight HTTP request to check if a webhook URL
// is reachable. Returns the response status, content type and body (capped at
// maxTestResponseBody), or any error.
func (d *Dispatcher) TestConnectivity(ctx context.Context, url, method string) (TestResult, error) {
	testCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if method == "" {
		method = http.MethodPost
	}

	req, err := http.NewRequestWithContext(testCtx, method, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return TestResult{}, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", d.userAgent)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.client.Do(req)
	if err != nil {
		return TestResult{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTestResponseBody+1))
	if err != nil {
		return TestResult{}, fmt.Errorf("reading response: %w", err)
	}
	result := TestResult{StatusCode: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}
	if len(body) > maxTestResponseBody {
		body = body[:maxTestResponseBody]
		result.Truncated = true
	}
	result.Body = string(body)
	return result, nil
}

type webhookError struct {
	statusCode   int
	responseBody string
}

func (e *webhookError) Error() string {
	return fmt.Sprintf("webhook returned status %d", e.statusCode)
}
