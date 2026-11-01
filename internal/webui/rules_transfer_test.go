package webui

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
)

func TestRulesExportResponses(t *testing.T) {
	env := newTestEnv(t)
	for _, tc := range []struct {
		query, contentType, filename string
		status                       int
	}{
		{"", "application/yaml", "rules.yaml", http.StatusOK},
		{"?format=json", "application/json", "rules.json", http.StatusOK},
		{"?format=xml", "text/plain; charset=utf-8", "", http.StatusBadRequest},
	} {
		t.Run(tc.query, func(t *testing.T) {
			rec := env.do(t, http.MethodGet, "/rules/export"+tc.query, nil, false)
			if rec.Code != tc.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Content-Type") != tc.contentType {
				t.Fatal(rec.Header())
			}
			if tc.status != http.StatusOK {
				return
			}
			if rec.Header().Get("Cache-Control") != "no-store" || !strings.Contains(rec.Header().Get("Content-Disposition"), tc.filename) {
				t.Fatal(rec.Header())
			}
			items, err := rules.Decode(rec.Body.Bytes())
			if err != nil || len(items) != 0 {
				t.Fatalf("empty export: %v %v", items, err)
			}
			if !strings.Contains(rec.Body.String(), "Contains secrets") {
				t.Fatal("missing sensitivity warning")
			}
		})
	}
}

func TestRulesImportDiffAndApply(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	original := models.RuleRecord{Name: "existing", Enabled: true, Position: 1, Match: models.MatcherConfig{Mode: "all"}, Webhook: models.WebhookConfig{URL: "https://example.com", Method: "POST"}}
	if err := env.store.SaveRule(ctx, &original); err != nil {
		t.Fatal(err)
	}
	if err := env.h.LoadRules(ctx); err != nil {
		t.Fatal(err)
	}
	updated := original
	updated.Enabled = false
	updated.Position = 5
	updated.Match.Conditions = []models.MatchCondition{{Field: "subject", Pattern: "invoice*"}}
	updated.Webhook.URL = "https://example.com/new?token=secret"
	data, err := rules.Export([]models.RuleRecord{updated}, "yaml")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/rules/import?mode=replace", strings.NewReader(string(data)))
	rec := httptest.NewRecorder()
	env.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
	}
	var changes []importChange
	if err := json.Unmarshal(rec.Body.Bytes(), &changes); err != nil {
		t.Fatal(err)
	}
	want := []importChange{{Name: "existing", Action: "updated", Fields: []string{"enabled", "position", "match", "webhook"}}}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("diff = %#v", changes)
	}
	if strings.Contains(rec.Body.String(), "token=secret") {
		t.Fatal("preview revealed credential value")
	}
	stored, err := env.store.GetRule(ctx, original.ID)
	if err != nil || !reflect.DeepEqual(*stored, original) {
		t.Fatalf("preview changed rule: %v", err)
	}
	// Empty replace must be an explicit apply; preview cannot clear rules.
	for _, apply := range []bool{false, true} {
		target := "/rules/import?mode=replace"
		if apply {
			target += "&apply=true"
		}
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, strings.NewReader("version: 1\nrules: []")))
		if rec.Code != http.StatusOK {
			t.Fatalf("replace: %d %s", rec.Code, rec.Body.String())
		}
		stored, err := env.store.ListRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if (!apply && len(stored) != 1) || (apply && len(stored) != 0) {
			t.Fatalf("apply=%v, rules=%v", apply, stored)
		}
	}
	if matched := env.h.engine.Match(&models.ParsedEmail{}); len(matched) != 0 {
		t.Fatal("import did not reload engine")
	}
}

type failedRulesBody struct{}

func (failedRulesBody) Read([]byte) (int, error) { return 0, errors.New("upload interrupted") }
func (failedRulesBody) Close() error             { return nil }

func TestRulesImportBadRequests(t *testing.T) {
	env := newTestEnv(t)
	for _, tc := range []struct {
		name, target, want string
		body               io.ReadCloser
	}{
		{"oversized", "/rules/import?mode=merge", "exceeds 2 MiB", io.NopCloser(strings.NewReader(strings.Repeat(" ", (2<<20)+1)))},
		{"interrupted", "/rules/import?mode=merge", "could not be read", failedRulesBody{}},
		{"invalid mode", "/rules/import?mode=invalid", "mode must be merge or replace", io.NopCloser(strings.NewReader("version: 1\nrules: []"))},
		{"malformed", "/rules/import?mode=merge", "expected version", io.NopCloser(strings.NewReader("version: 2\nrules: []"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tc.target, tc.body))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("error response can be cached")
			}
		})
	}
}

func TestRulesTransferClosedDatabase(t *testing.T) {
	env := newTestEnv(t)
	if err := env.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/rules/export", ""},
		{http.MethodPost, "/rules/import?mode=merge", "version: 1\nrules: []"},
	} {
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "Could not read rules") {
			t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestRulesImportDatabaseErrors(t *testing.T) {
	for _, tc := range []struct {
		name, trigger, want string
		saved               bool
	}{
		{"write fails", `CREATE TRIGGER reject_import BEFORE INSERT ON rules BEGIN SELECT RAISE(ABORT, 'write denied'); END`, "Could not import rules", false},
		{"reload fails", `CREATE TRIGGER corrupt_import AFTER INSERT ON rules BEGIN UPDATE rules SET match_config = 'invalid json' WHERE id = new.id; END`, "Rules saved; reload failed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			// A separate SQLite connection injects storage failures without changing production code.
			db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(env.rawDir), "test.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(tc.trigger); err != nil {
				t.Fatal(err)
			}
			data, err := rules.Export([]models.RuleRecord{{Name: "import", Enabled: true, Webhook: models.WebhookConfig{URL: "https://example.com"}}}, "json")
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			env.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/rules/import?mode=merge&apply=true", strings.NewReader(string(data))))
			if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("response: %d %s", rec.Code, rec.Body.String())
			}
			var count int
			if err := db.QueryRow("SELECT COUNT(*) FROM rules").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (count == 1) != tc.saved {
				t.Fatalf("rules persisted: %d, want saved=%v", count, tc.saved)
			}
		})
	}
}

func TestRuleEditorUsesImportValidation(t *testing.T) {
	form := url.Values{"name": {"editor"}, "webhook_url": {"https://example.com"}, "cond_field": {"subject"}, "cond_pattern": {"/[/"}}
	req := httptest.NewRequest(http.MethodPost, "/rules", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := req.ParseForm(); err != nil {
		t.Fatal(err)
	}
	rule, msg := parseRuleForm(req)
	if rule == nil || !strings.Contains(msg, "invalid pattern") {
		t.Fatalf("editor validation: %v %q", rule, msg)
	}
}
