package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gowthamgts/mailrelay/internal/rules"
	playwright "github.com/mxschmitt/playwright-go"
)

func TestRulesCLI(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("webui:\n  db_path: "+filepath.Join(dir, "rules.db")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(dir, "input.yaml")
	data := []byte("version: 1\nrules:\n  - name: cli\n    enabled: true\n    position: 2\n    match: {}\n    webhook:\n      url: https://example.com/hook\n      method: POST\n")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	runCLI := func(args ...string) ([]byte, error) {
		var out bytes.Buffer
		err := run(context.Background(), append([]string{"-config", cfg}, args...), &out)
		return out.Bytes(), err
	}
	if _, err := runCLI("-import-rules", input); err != nil {
		t.Fatal(err)
	}
	for _, dest := range []string{"-", filepath.Join(dir, "export.json")} {
		out, err := runCLI("-export-rules", dest)
		if err != nil {
			t.Fatal(err)
		}
		if dest != "-" {
			out, err = os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			info, _ := os.Stat(dest)
			if info.Mode().Perm() != 0600 {
				t.Fatal("insecure file permissions")
			}
		}
		items, err := rules.Decode(out)
		if err != nil || len(items) != 1 || items[0].Name != "cli" {
			t.Fatalf("bad export: %s %v", out, err)
		}
	}
	for _, args := range [][]string{{"-replace"}, {"-export-rules", "-", "-import-rules", input}, {"-export-rules", filepath.Join(dir, "export.json")}} {
		if _, err := runCLI(args...); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	empty := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(empty, []byte("version: 1\nrules: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runCLI("-import-rules", empty, "-replace"); err != nil {
		t.Fatal(err)
	}
	out, err := runCLI("-export-rules", "-")
	if err != nil {
		t.Fatal(err)
	}
	items, err := rules.Decode(out)
	if err != nil || len(items) != 0 {
		t.Fatal("replace failed")
	}
}

func TestUI_RulesImportPreview(t *testing.T) {
	browser := pwSetup(t)
	for _, theme := range []string{"light", "dark"} {
		t.Run(theme, func(t *testing.T) {
			h := newUIHarness(t)
			page := newPage(t, browser)
			page.AddInitScript(playwright.Script{Content: playwright.String("localStorage.setItem('theme', '" + theme + "')")})
			if _, err := page.Goto(h.url("/rules")); err != nil {
				t.Fatal(err)
			}
			if err := page.Locator("summary").Click(); err != nil {
				t.Fatal(err)
			}
			input := filepath.Join(t.TempDir(), "rules.yaml")
			os.WriteFile(input, []byte("version: 1\nrules:\n  - name: browser import\n    enabled: true\n    position: 0\n    match: {}\n    webhook:\n      url: https://example.com/hook\n      method: POST\n"), 0600)
			if err := page.Locator("#rules-file").SetInputFiles(input); err != nil {
				t.Fatal(err)
			}
			if err := page.Locator("#preview-import").Click(); err != nil {
				t.Fatal(err)
			}
			waitForSelector(t, page, "#import-status:has-text('created: browser import')")
			records, _ := h.store.ListRules(context.Background())
			if len(records) != 0 {
				t.Fatal("preview wrote rules")
			}
			if err := page.Locator("#apply-import").Click(); err != nil {
				t.Fatal(err)
			}
			waitForSelector(t, page, "[data-rule-name='browser import']")
			if err := page.Locator("summary").Click(); err != nil {
				t.Fatal(err)
			}
			page.Locator("#rules-file").SetInputFiles(input)
			page.Locator("#preview-import").Click()
			waitForSelector(t, page, "#import-status:has-text('unchanged: browser import')")
			page.Locator("#import-mode").SelectOption(playwright.SelectOptionValues{Values: &[]string{"replace"}})
			visible, err := page.Locator("#apply-import").IsVisible()
			if err != nil || visible {
				t.Fatal("mode change did not invalidate preview")
			}
		})
	}
}
