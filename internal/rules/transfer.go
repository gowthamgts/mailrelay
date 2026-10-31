package rules

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/webhook"
	"go.yaml.in/yaml/v3"
)

// TransferRule contains portable rule settings, without database identity or timestamps.
type TransferRule struct {
	Name     string               `json:"name"`
	Enabled  bool                 `json:"enabled"`
	Position int                  `json:"position"`
	Match    models.MatcherConfig `json:"match"`
	Webhook  models.WebhookConfig `json:"webhook"`
}

type RulesFile struct {
	Version   int            `json:"version"`
	Sensitive string         `json:"sensitive,omitempty"`
	Rules     []TransferRule `json:"rules"`
}

func Portable(r models.RuleRecord) TransferRule {
	return TransferRule{r.Name, r.Enabled, r.Position, r.Match, r.Webhook}
}

func Export(records []models.RuleRecord, format string) ([]byte, error) {
	f := RulesFile{Version: 1, Sensitive: "Contains secrets: webhook URLs, headers and payload templates may contain credentials. Store securely.", Rules: []TransferRule{}}
	for _, r := range records {
		f.Rules = append(f.Rules, Portable(r))
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil || format == "json" {
		return b, err
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		return nil, err
	}
	return yaml.Marshal(value)
}

func Decode(data []byte) ([]TransferRule, error) {
	var value any
	d := yaml.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected one YAML or JSON document")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var f RulesFile
	jd := json.NewDecoder(bytes.NewReader(b))
	jd.DisallowUnknownFields()
	if err := jd.Decode(&f); err != nil {
		return nil, err
	}
	if f.Version != 1 || f.Rules == nil {
		return nil, fmt.Errorf("expected version: 1 and a rules list")
	}
	if err := ValidateImport(f.Rules); err != nil {
		return nil, err
	}
	return f.Rules, nil
}

func ValidateImport(items []TransferRule) error {
	seen := map[string]bool{}
	for _, item := range items {
		r := models.RuleRecord{Name: item.Name, Position: item.Position, Match: item.Match, Webhook: item.Webhook}
		if err := Validate(&r); err != nil {
			return fmt.Errorf("rule %q: %w", item.Name, err)
		}
		if seen[item.Name] {
			return fmt.Errorf("duplicate rule name %q", item.Name)
		}
		seen[item.Name] = true
	}
	return nil
}

// Validate is shared by the editor and every import entry point.
func Validate(r *models.RuleRecord) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("rule name is required")
	}
	if r.Position < 0 {
		return fmt.Errorf("position must be non-negative")
	}
	u, err := url.Parse(r.Webhook.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("webhook URL must be an absolute HTTP or HTTPS URL")
	}
	if _, err := http.NewRequest(r.Webhook.Method, r.Webhook.URL, nil); err != nil {
		return fmt.Errorf("invalid webhook method or URL: %w", err)
	}
	for k, v := range r.Webhook.Headers {
		if k == "" || strings.ContainsAny(v, "\r\n") {
			return fmt.Errorf("invalid webhook header")
		}
		for _, c := range k {
			if !strings.ContainsRune("!#$%&'*+-.^_`|~0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", c) {
				return fmt.Errorf("invalid webhook header name %q", k)
			}
		}
	}
	if strings.TrimSpace(r.Webhook.PayloadTemplate) != "" {
		if _, err := webhook.ParseTemplate(r.Webhook.PayloadTemplate); err != nil {
			return fmt.Errorf("invalid payload template: %w", err)
		}
	}
	for _, n := range []*int{r.Webhook.Timeout, r.Webhook.MaxRetries} {
		if n != nil && *n < 0 {
			return fmt.Errorf("timeout and max retries must be non-negative")
		}
	}
	for _, n := range []*int{r.Webhook.InitialWait, r.Webhook.MaxWait} {
		if n != nil && *n < 1 {
			return fmt.Errorf("retry waits must be at least 1 second")
		}
	}
	if r.Webhook.InitialWait != nil && r.Webhook.MaxWait != nil && *r.Webhook.InitialWait > *r.Webhook.MaxWait {
		return fmt.Errorf("initial wait cannot exceed max wait")
	}
	if r.Match.Mode != "" && r.Match.Mode != "all" && r.Match.Mode != "any" {
		return fmt.Errorf("match mode must be all or any")
	}
	for _, c := range r.Match.Conditions {
		switch c.Field {
		case "from", "to", "cc", "mail_from", "rcpt_to", "subject", "from_domain", "to_domain", "mail_from_domain", "rcpt_to_domain", "header", "body":
		default:
			return fmt.Errorf("unknown match field %q", c.Field)
		}
		if c.Field == "header" && strings.TrimSpace(c.Header) == "" {
			return fmt.Errorf("header conditions require a header name")
		}
		p := strings.TrimSpace(c.Pattern)
		if p == "" {
			return fmt.Errorf("condition pattern is required")
		}
		var err error
		if len(p) >= 2 && p[0] == '/' && p[len(p)-1] == '/' {
			_, err = regexp.Compile(p[1 : len(p)-1])
		} else {
			_, err = path.Match(p, "")
		}
		if err != nil {
			return fmt.Errorf("invalid pattern %q: %w", p, err)
		}
	}
	return nil
}
