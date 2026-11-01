package rules

import (
	"strings"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestImportValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*models.RuleRecord)
	}{
		{"regex", func(r *models.RuleRecord) {
			r.Match.Conditions = []models.MatchCondition{{Field: "subject", Pattern: "/[/"}}
		}},
		{"glob", func(r *models.RuleRecord) {
			r.Match.Conditions = []models.MatchCondition{{Field: "from", Pattern: "["}}
		}},
		{"field", func(r *models.RuleRecord) {
			r.Match.Conditions = []models.MatchCondition{{Field: "unknown", Pattern: "*"}}
		}},
		{"template", func(r *models.RuleRecord) { r.Webhook.PayloadTemplate = "{{ broken }}" }},
		{"url", func(r *models.RuleRecord) { r.Webhook.URL = "/relative" }},
		{"header", func(r *models.RuleRecord) { r.Webhook.Headers = map[string]string{"Authorization": "x\r\ny"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := models.RuleRecord{Name: "test", Webhook: models.WebhookConfig{URL: "https://example.com", Method: "POST"}}
			tc.change(&r)
			if Validate(&r) == nil {
				t.Fatal("expected rejection")
			}
		})
	}
	for _, input := range []string{`version: 2
rules: []`, `version: 1
rules: []
unknown: true`, `version: 1
rules: []
---
rules: []`, `version: 1
rules: []
rules: []`} {
		if _, err := Decode([]byte(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	r := TransferRule{Name: "same", Webhook: models.WebhookConfig{URL: "https://example.com"}}
	if err := ValidateImport([]TransferRule{r, r}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatal(err)
	}
}

func TestImportValidationBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*models.RuleRecord)
		want   string
	}{
		{"blank name", func(r *models.RuleRecord) { r.Name = " \t" }, "rule name is required"},
		{"negative position", func(r *models.RuleRecord) { r.Position = -1 }, "position must be non-negative"},
		{"invalid method", func(r *models.RuleRecord) { r.Webhook.Method = "POST GET" }, "invalid webhook method"},
		{"invalid header name", func(r *models.RuleRecord) { r.Webhook.Headers = map[string]string{"Bad Header": "value"} }, "invalid webhook header name"},
		{"empty header name", func(r *models.RuleRecord) { r.Webhook.Headers = map[string]string{"": "value"} }, "invalid webhook header"},
		{"negative timeout", func(r *models.RuleRecord) { n := -1; r.Webhook.Timeout = &n }, "timeout and max retries must be non-negative"},
		{"negative retries", func(r *models.RuleRecord) { n := -1; r.Webhook.MaxRetries = &n }, "timeout and max retries must be non-negative"},
		{"zero initial wait", func(r *models.RuleRecord) { n := 0; r.Webhook.InitialWait = &n }, "retry waits must be at least 1 second"},
		{"negative max wait", func(r *models.RuleRecord) { n := -1; r.Webhook.MaxWait = &n }, "retry waits must be at least 1 second"},
		{"inverted waits", func(r *models.RuleRecord) { a, b := 2, 1; r.Webhook.InitialWait, r.Webhook.MaxWait = &a, &b }, "initial wait cannot exceed max wait"},
		{"unknown mode", func(r *models.RuleRecord) { r.Match.Mode = "none" }, "match mode must be all or any"},
		{"missing header name", func(r *models.RuleRecord) {
			r.Match.Conditions = []models.MatchCondition{{Field: "header", Pattern: "*"}}
		}, "header conditions require a header name"},
		{"blank pattern", func(r *models.RuleRecord) {
			r.Match.Conditions = []models.MatchCondition{{Field: "subject", Pattern: " \t"}}
		}, "condition pattern is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := models.RuleRecord{Name: "test", Webhook: models.WebhookConfig{URL: "https://example.com", Method: "POST"}}
			tc.change(&r)
			err := ValidateImport([]TransferRule{Portable(r)})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
	// Zero disables timeouts/retries; waits accept one second and may be equal.
	zero, one := 0, 1
	r := models.RuleRecord{Name: "boundaries", Webhook: models.WebhookConfig{URL: "http://example.com", Timeout: &zero, MaxRetries: &zero, InitialWait: &one, MaxWait: &one}, Match: models.MatcherConfig{Mode: "all", Conditions: []models.MatchCondition{{Field: "header", Header: "X-Source", Pattern: "alerts*"}}}}
	if err := Validate(&r); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeRejectsMalformedFiles(t *testing.T) {
	for _, input := range []string{
		"version: [",                         // Invalid YAML.
		"? [complex, key]\n: value",          // Non-string map key cannot become JSON.
		"version: 1\nrules: []\nextra: .nan", // Non-finite YAML numbers cannot become JSON.
		"version: 1\nrules: null",            // Do not accidentally interpret null as replace-all.
		"version: 1\nrules:\n  - name: invalid\n    webhook:\n      url: /relative",
		"version: 1\nrules:\n  - name: invalid\n    webhook:\n      url: https://example.com\n      unknown: true",
	} {
		t.Run(input, func(t *testing.T) {
			if _, err := Decode([]byte(input)); err == nil {
				t.Fatal("invalid file accepted")
			}
		})
	}
}
