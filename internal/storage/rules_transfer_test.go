package storage

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestRulesTransfer(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	n := 7
	enabled := false
	source := []models.TransferRule{{Name: "alerts", Enabled: true, Position: 3, Match: models.MatcherConfig{Mode: "any", Conditions: []models.MatchCondition{{Field: "subject", Pattern: "/invoice/i/"}}}, Webhook: models.WebhookConfig{URL: "https://example.com/hook?token=secret", Method: "PUT", Headers: map[string]string{"Authorization": "Bearer secret"}, PayloadTemplate: `{"text": {{ .Subject }}}`, Timeout: &n, RetryOnTimeout: &enabled}}}
	if err := s.ImportRules(ctx, source, false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		records, err := s.ListRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var decoded []models.TransferRule
		for _, r := range records {
			decoded = append(decoded, models.TransferRule{Name: r.Name, Enabled: r.Enabled, Position: r.Position, Match: r.Match, Webhook: r.Webhook})
		}
		if !reflect.DeepEqual(decoded, source) {
			t.Fatalf("round trip: %#v", decoded)
		}
		if err := s.ImportRules(ctx, decoded, true); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.ListRules(ctx)
	other := models.TransferRule{Name: "other", Enabled: false, Position: 8, Webhook: models.WebhookConfig{URL: "https://example.com/other", Method: "POST"}}
	if err := s.ImportRules(ctx, []models.TransferRule{other}, false); err != nil {
		t.Fatal(err)
	}
	records, _ := s.ListRules(ctx)
	if len(records) != 2 {
		t.Fatal("merge removed a rule")
	}
	source[0].Enabled = false
	if err := s.ImportRules(ctx, source, false); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetRule(ctx, before[0].ID)
	if err != nil || current.Enabled {
		t.Fatalf("merge did not update existing identity: %v", err)
	}
	baseline, _ := s.ListRules(ctx)
	// A database error after the first insert must roll back that insert and deletion.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_import BEFORE INSERT ON rules WHEN new.name = 'fail' BEGIN SELECT RAISE(ABORT, 'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	fail := other
	fail.Name = "fail"
	if err := s.ImportRules(ctx, []models.TransferRule{other, fail}, true); err == nil {
		t.Fatal("expected transaction error")
	}
	records, _ = s.ListRules(ctx)
	if !reflect.DeepEqual(records, baseline) {
		t.Fatal("transaction did not roll back")
	}
	if err := s.ImportRules(ctx, []models.TransferRule{other}, true); err != nil {
		t.Fatal(err)
	}
	records, _ = s.ListRules(ctx)
	if len(records) != 1 || records[0].Name != "other" {
		t.Fatal("replace kept absent rule")
	}
	if err := s.ImportRules(ctx, []models.TransferRule{}, true); err != nil {
		t.Fatal(err)
	}
	records, _ = s.ListRules(ctx)
	if len(records) != 0 {
		t.Fatal("empty replace did not clear rules")
	}
}

func TestRulesImportDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	original := models.TransferRule{Name: "original", Enabled: true, Webhook: models.WebhookConfig{URL: "https://example.com"}}
	if err := s.ImportRules(ctx, []models.TransferRule{original}, false); err != nil {
		t.Fatal(err)
	}
	before, err := s.ListRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_delete BEFORE DELETE ON rules BEGIN SELECT RAISE(ABORT, 'deletion denied'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRules(ctx, []models.TransferRule{}, true); err == nil {
		t.Fatal("expected deletion failure")
	}
	after, err := s.ListRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed replace changed existing rules")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.ImportRules(ctx, []models.TransferRule{original}, false); err == nil {
		t.Fatal("expected closed database failure")
	}
}
