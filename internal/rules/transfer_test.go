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
