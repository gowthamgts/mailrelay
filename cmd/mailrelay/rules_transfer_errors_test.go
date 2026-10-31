package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/storage"
)

func TestRulesCLIImportFileErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := &models.AppConfig{WebUI: models.WebUIConfig{DBPath: filepath.Join(dir, "rules.db")}}
	store, err := storage.Open(cfg.WebUI.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	original := models.RuleRecord{Name: "original", Enabled: true, Webhook: models.WebhookConfig{URL: "https://example.com"}}
	if err := store.SaveRule(ctx, &original); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, content, want string }{
		{"missing", "", "no such file"},
		{"directory", "", "directory"},
		{"oversized", strings.Repeat(" ", (2<<20)+1), "rules file exceeds 2 MiB"},
		{"malformed", "version: [", "yaml"},
		{"invalid rule", "version: 1\nrules:\n  - name: invalid\n    webhook:\n      url: /relative", "webhook URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(dir, tc.name)
			switch tc.name {
			case "missing":
			case "directory":
				if err := os.Mkdir(input, 0700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.WriteFile(input, []byte(tc.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := transferRules(ctx, cfg, "", input, true, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			records, err := store.ListRules(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 1 || !reflect.DeepEqual(records[0], original) {
				t.Fatal("invalid CLI replace changed stored rules")
			}
		})
	}
}

type failedRulesWriter struct{ err error }

func (w failedRulesWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRulesCLIExportErrors(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfg := &models.AppConfig{WebUI: models.WebUIConfig{DBPath: filepath.Join(dir, "rules.db")}}
	sentinel := errors.New("stdout closed")
	if err := transferRules(ctx, cfg, "-", "", false, failedRulesWriter{sentinel}); !errors.Is(err, sentinel) {
		t.Fatalf("stdout error lost: %v", err)
	}
	if err := transferRules(ctx, cfg, filepath.Join(dir, "missing", "export.yaml"), "", false, io.Discard); err == nil {
		t.Fatal("export to missing directory succeeded")
	}
	badCfg := &models.AppConfig{WebUI: models.WebUIConfig{DBPath: filepath.Join(dir, "missing", "rules.db")}}
	if err := transferRules(ctx, badCfg, "-", "", false, io.Discard); err == nil {
		t.Fatal("export with unavailable database succeeded")
	}
	// An invalid stored matcher must prevent a corrupt backup from being created.
	db, err := sql.Open("sqlite", cfg.WebUI.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO rules (id,name,enabled,match_config,webhook_config,position,created_at,updated_at) VALUES ('broken','broken',1,'not json','{}',0,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "corrupt.yaml")
	if err := transferRules(ctx, cfg, dest, "", false, io.Discard); err == nil || !strings.Contains(err.Error(), "decoding match") {
		t.Fatalf("corrupt rule export: %v", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed export created a file: %v", err)
	}
}
