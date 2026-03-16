package models

import (
	"net/mail"
	"strings"
	"time"
)

// ParsedEmail represents a fully parsed email message.
type ParsedEmail struct {
	From        string       `json:"from"`
	To          []string     `json:"to"`
	CC          []string     `json:"cc,omitempty"`
	Subject     string       `json:"subject"`
	TextBody    string       `json:"text_body,omitempty"`
	HTMLBody    string       `json:"html_body,omitempty"`
	Headers     mail.Header  `json:"headers,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	AuthResult  *AuthResult  `json:"auth_result,omitempty"`

	// Envelope addresses from SMTP transaction (canonical).
	EnvelopeFrom string   `json:"envelope_from"`
	EnvelopeTo   []string `json:"envelope_to"`
}

// FromDomain extracts the domain part of the From address.
func (e *ParsedEmail) FromDomain() string {
	parts := strings.SplitN(e.EnvelopeFrom, "@", 2)
	if len(parts) == 2 {
		return strings.ToLower(parts[1])
	}
	return ""
}

// ToDomains returns deduplicated domains from the envelope To addresses.
func (e *ParsedEmail) ToDomains() []string {
	seen := make(map[string]struct{})
	var domains []string
	for _, addr := range e.EnvelopeTo {
		parts := strings.SplitN(addr, "@", 2)
		if len(parts) == 2 {
			d := strings.ToLower(parts[1])
			if _, ok := seen[d]; !ok {
				seen[d] = struct{}{}
				domains = append(domains, d)
			}
		}
	}
	return domains
}

// Attachment represents an email attachment.
type Attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Content     string `json:"content"` // base64-encoded
}

// AuthCheckResult represents the outcome of a single auth check.
type AuthCheckResult string

const (
	AuthPass AuthCheckResult = "pass"
	AuthFail AuthCheckResult = "fail"
	AuthNone AuthCheckResult = "none"
)

// AuthResult holds the results of SPF, DKIM, DMARC, and ARC verification.
type AuthResult struct {
	SPF   AuthCheckResult `json:"spf"`
	DKIM  AuthCheckResult `json:"dkim"`
	DMARC AuthCheckResult `json:"dmarc"`
	ARC   AuthCheckResult `json:"arc"`
}

// EmailStatus represents the lifecycle state of a processed email.
type EmailStatus string

const (
	EmailStatusReceived       EmailStatus = "received"
	EmailStatusDropped        EmailStatus = "dropped"
	EmailStatusDelivered      EmailStatus = "delivered"
	EmailStatusPartialFailure EmailStatus = "partial_failure"
	EmailStatusFailed         EmailStatus = "failed"
	EmailStatusRejected       EmailStatus = "rejected"
)

// EmailRecord is a persisted email with its processing status.
type EmailRecord struct {
	ID              string       `json:"id"`
	ReceivedAt      time.Time    `json:"received_at"`
	EnvelopeFrom    string       `json:"envelope_from"`
	EnvelopeTo      []string     `json:"envelope_to"`
	Subject         string       `json:"subject"`
	Status          EmailStatus  `json:"status"`
	RejectionReason string       `json:"rejection_reason,omitempty"`
	ParsedEmail     *ParsedEmail `json:"parsed_email,omitempty"`
	AuthResult      *AuthResult  `json:"auth_result,omitempty"`
}

// DeliveryRecord tracks a single webhook delivery attempt for an email.
type DeliveryRecord struct {
	ID           string    `json:"id"`
	EmailID      string    `json:"email_id"`
	RuleName     string    `json:"rule_name"`
	Status       string    `json:"status"`
	StatusCode   int       `json:"status_code"`
	ErrorMessage string    `json:"error_message,omitempty"`
	ResponseBody string    `json:"response_body,omitempty"`
	Attempts     int       `json:"attempts"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// DeliveryResult is returned by the dispatcher after attempting a webhook.
type DeliveryResult struct {
	RuleName     string
	Status       string // "success", "failed", "rejected", "error", "cancelled"
	StatusCode   int
	Error        string
	ResponseBody string
	Attempts     int
}

// WebUIConfig controls the web UI and its backing store.
type WebUIConfig struct {
	Enabled       bool   `json:"enabled" koanf:"enabled"`
	DBPath        string `json:"db_path" koanf:"db_path"`
	RetentionDays int    `json:"retention_days" koanf:"retention_days"`
	RawEmailDir   string `json:"raw_email_dir" koanf:"raw_email_dir"`
}

// MatcherConfig defines the matching criteria for a rule.
type MatcherConfig struct {
	ToEmail    string `json:"to_email"`
	FromEmail  string `json:"from_email"`
	Subject    string `json:"subject"`
	FromDomain string `json:"from_domain"`
	ToDomain   string `json:"to_domain"`
}

// WebhookConfig defines the webhook target for a rule.
type WebhookConfig struct {
	URL             string            `json:"url"`
	Method          string            `json:"method"`
	Headers         map[string]string `json:"headers,omitempty"`
	PayloadTemplate string            `json:"payload_template,omitempty"`
}

// Rule pairs a matcher with a webhook action.
type Rule struct {
	Name    string        `json:"name"`
	Match   MatcherConfig `json:"match"`
	Webhook WebhookConfig `json:"webhook"`
}

// RuleRecord is a rule persisted in the database (user-managed via the web UI).
type RuleRecord struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Enabled   bool          `json:"enabled"`
	Match     MatcherConfig `json:"match"`
	Webhook   WebhookConfig `json:"webhook"`
	Position  int           `json:"position"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// ToRule converts a RuleRecord to a Rule for use in the matching engine.
func (r *RuleRecord) ToRule() Rule {
	return Rule{
		Name:    r.Name,
		Match:   r.Match,
		Webhook: r.Webhook,
	}
}

// RetryConfig controls exponential backoff for webhook delivery.
type RetryConfig struct {
	MaxRetries  int           `json:"max_retries" koanf:"max_retries"`
	InitialWait time.Duration `json:"initial_wait" koanf:"initial_wait"`
	MaxWait     time.Duration `json:"max_wait" koanf:"max_wait"`
}

// SMTPConfig defines SMTP server settings.
type SMTPConfig struct {
	Addr              string        `json:"addr" koanf:"addr"`
	Domain            string        `json:"domain" koanf:"domain"`
	MaxMessageBytes   int64         `json:"max_message_bytes" koanf:"max_message_bytes"`
	MaxRecipients     int           `json:"max_recipients" koanf:"max_recipients"`
	ReadTimeout       time.Duration `json:"read_timeout" koanf:"read_timeout"`
	WriteTimeout      time.Duration `json:"write_timeout" koanf:"write_timeout"`
	AllowedRecipients []string      `json:"allowed_recipients,omitempty" koanf:"allowed_recipients"`
}

// AuthMode controls the behavior of a single auth check (SPF, DKIM, or DMARC).
//   - "off"     — skip the check entirely
//   - "log"     — run the check, log the result, but never reject
//   - "enforce" — run the check and reject the email on failure (SMTP 550)
type AuthMode string

const (
	AuthModeOff     AuthMode = "off"
	AuthModeLog     AuthMode = "log"
	AuthModeEnforce AuthMode = "enforce"
)

func (m AuthMode) Enabled() bool  { return m != AuthModeOff }
func (m AuthMode) Enforced() bool { return m == AuthModeEnforce }

// AuthConfig controls email authentication verification.
// ARC is not configurable — it is always verified to correctly handle
// forwarded emails (see RFC 8617).
type AuthConfig struct {
	SPF   AuthMode `json:"spf" koanf:"spf"`
	DKIM  AuthMode `json:"dkim" koanf:"dkim"`
	DMARC AuthMode `json:"dmarc" koanf:"dmarc"`
}

// HTTPAuthProviderConfig represents a single OIDC/OAuth2 provider used to
// authenticate access to the HTTP server and web UI.
type HTTPAuthProviderConfig struct {
	// Name is a short identifier for the provider (eg. "google", "github").
	Name string `json:"name" koanf:"name"`
	// IssuerURL is the OIDC issuer URL (eg. https://accounts.google.com).
	IssuerURL string `json:"issuer_url" koanf:"issuer_url"`
	// ClientID is the OAuth2 client ID registered with the provider.
	ClientID string `json:"client_id" koanf:"client_id"`
	// ClientSecret is the OAuth2 client secret registered with the provider.
	ClientSecret string `json:"client_secret" koanf:"client_secret"`
	// Scopes are additional scopes to request. "openid", "email", and "profile"
	// are always included by default.
	Scopes []string `json:"scopes,omitempty" koanf:"scopes"`
}

// HTTPAuthConfig controls OIDC-based authentication for the HTTP server.
type HTTPAuthConfig struct {
	// Enabled toggles HTTP authentication. When false, all HTTP endpoints
	// behave as they do today (no login required).
	Enabled bool `json:"enabled" koanf:"enabled"`
	// Providers is the list of configured OIDC/OAuth2 providers. The first
	// provider is used as the default when a specific provider is not
	// explicitly requested.
	Providers []HTTPAuthProviderConfig `json:"providers,omitempty" koanf:"providers"`
	// AllowedEmailDomains optionally restricts access to users whose primary
	// email domain matches one of the configured domains (case-insensitive).
	AllowedEmailDomains []string `json:"allowed_email_domains,omitempty" koanf:"allowed_email_domains"`
	// CookieName is the name of the session cookie that tracks logged-in users.
	CookieName string `json:"cookie_name" koanf:"cookie_name"`
	// CookieSecure controls the Secure flag on the session cookie. When empty,
	// a safe default is chosen based on the HTTP listen address.
	CookieSecure *bool `json:"cookie_secure,omitempty" koanf:"cookie_secure"`
	// CookieDomain optionally sets the cookie domain.
	CookieDomain string `json:"cookie_domain,omitempty" koanf:"cookie_domain"`
	// CookieSameSite controls SameSite behavior for the cookie. Supported
	// values are "lax", "strict", and "none" (case-insensitive).
	CookieSameSite string `json:"cookie_samesite,omitempty" koanf:"cookie_samesite"`
	// SessionTTL is the maximum lifetime of a session.
	SessionTTL time.Duration `json:"session_ttl" koanf:"session_ttl"`
	// SessionSecret optionally overrides the random per-process session key
	// with a stable secret, allowing multiple instances to share sessions.
	SessionSecret string `json:"session_secret,omitempty" koanf:"session_secret"`
}

// HTTPConfig defines the HTTP server (serves health checks, metrics, and the web UI).
type HTTPConfig struct {
	Addr          string         `json:"addr" koanf:"addr"`
	Auth          HTTPAuthConfig `json:"auth" koanf:"auth"`
	ProtectMetrics bool           `json:"protect_metrics,omitempty" koanf:"protect_metrics"`
}

// AppConfig is the top-level configuration.
type AppConfig struct {
	LogLevel string      `json:"log_level" koanf:"log_level"`
	SMTP     SMTPConfig  `json:"smtp" koanf:"smtp"`
	Auth     AuthConfig  `json:"auth" koanf:"auth"`
	HTTP     HTTPConfig  `json:"http" koanf:"http"`
	Retry    RetryConfig `json:"retry" koanf:"retry"`
	WebUI    WebUIConfig `json:"webui" koanf:"webui"`
}
