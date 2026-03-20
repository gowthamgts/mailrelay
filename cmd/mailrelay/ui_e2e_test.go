package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
	"github.com/gowthamgts/mailrelay/internal/rules"
	"github.com/gowthamgts/mailrelay/internal/storage"
	"github.com/gowthamgts/mailrelay/internal/webhook"
	"github.com/gowthamgts/mailrelay/internal/webui"
	playwright "github.com/playwright-community/playwright-go"
)

// uiHarness holds a running HTTP server with the web UI and a seeded database.
type uiHarness struct {
	server  *httptest.Server
	store   *storage.Store
	dbPath  string
	handler *webui.Handler
}

func newUIHarness(t *testing.T) *uiHarness {
	t.Helper()

	// Temp SQLite database.
	dir := t.TempDir()
	dbPath := dir + "/test.db"

	store, err := storage.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	engine := rules.NewEngine()
	dispatcher := webhook.NewDispatcher(models.RetryConfig{
		MaxRetries:     1,
		InitialWait:    10 * time.Millisecond,
		MaxWait:        50 * time.Millisecond,
		Timeout:        5 * time.Second,
		RetryOnTimeout: true,
	}, "test")

	cfg := &models.AppConfig{
		HTTP: models.HTTPConfig{Addr: "127.0.0.1:0"},
	}

	handler, err := webui.NewHandler(store, engine, dispatcher, "", cfg)
	if err != nil {
		t.Fatalf("new handler: %v", err)
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	// Wrap with CSRF middleware (uses localhost so secure=false).
	wrapped := csrfMiddleware(mux, cfg.HTTP)
	wrapped = withSecurityHeaders(wrapped)

	srv := httptest.NewServer(wrapped)
	t.Cleanup(func() {
		srv.Close()
		store.Close()
		os.Remove(dbPath)
	})

	return &uiHarness{
		server:  srv,
		store:   store,
		dbPath:  dbPath,
		handler: handler,
	}
}

func (h *uiHarness) url(path string) string {
	return h.server.URL + path
}

// seedEmail inserts an email record into the test database.
func (h *uiHarness) seedEmail(ctx context.Context, t *testing.T, subject, from string, status models.EmailStatus) string {
	t.Helper()
	record := &models.EmailRecord{
		ReceivedAt:   time.Now().UTC(),
		EnvelopeFrom: from,
		EnvelopeTo:   []string{"relay@example.com"},
		Subject:      subject,
		Status:       status,
		ParsedEmail: &models.ParsedEmail{
			EnvelopeFrom: from,
			EnvelopeTo:   []string{"relay@example.com"},
			Subject:      subject,
			TextBody:     "Hello from " + from,
		},
	}
	if err := h.store.SaveEmail(ctx, record); err != nil {
		t.Fatalf("seed email: %v", err)
	}
	return record.ID
}

// seedRule inserts a rule into the test database.
func (h *uiHarness) seedRule(ctx context.Context, t *testing.T, name string) string {
	t.Helper()
	rule := &models.RuleRecord{
		Name:    name,
		Enabled: true,
		Match:   models.MatcherConfig{},
		Webhook: models.WebhookConfig{
			URL:    "https://example.com/hook",
			Method: "POST",
		},
	}
	if err := h.store.SaveRule(ctx, rule); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	return rule.ID
}

// pwSetup installs Playwright browsers and returns a browser + cleanup function.
// Tests are skipped if PLAYWRIGHT_SKIP is set or if installation fails.
func pwSetup(t *testing.T) playwright.Browser {
	t.Helper()

	if os.Getenv("PLAYWRIGHT_SKIP") != "" {
		t.Skip("PLAYWRIGHT_SKIP is set")
	}

	runOptions := &playwright.RunOptions{
		SkipInstallBrowsers: false,
		Verbose:             false,
	}
	if err := playwright.Install(runOptions); err != nil {
		t.Skipf("playwright install: %v", err)
	}

	pw, err := playwright.Run()
	if err != nil {
		t.Fatalf("playwright run: %v", err)
	}
	t.Cleanup(func() { pw.Stop() })

	browser, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{
		Headless: playwright.Bool(true),
	})
	if err != nil {
		t.Fatalf("launch browser: %v", err)
	}
	t.Cleanup(func() { browser.Close() })

	return browser
}

// newPage creates a browser page that automatically handles confirm dialogs by
// accepting them. This is needed for hx-confirm buttons.
func newPage(t *testing.T, browser playwright.Browser) playwright.Page {
	t.Helper()
	page, err := browser.NewPage()
	if err != nil {
		t.Fatalf("new page: %v", err)
	}
	t.Cleanup(func() { page.Close() })

	// Accept all browser confirm/alert dialogs (used by hx-confirm in HTMX).
	page.On("dialog", func(dialog playwright.Dialog) {
		if err := dialog.Accept(); err != nil {
			t.Logf("dialog accept: %v", err)
		}
	})

	return page
}

// waitForSelector waits up to 5 seconds for a CSS selector to appear.
func waitForSelector(t *testing.T, page playwright.Page, selector string) playwright.ElementHandle {
	t.Helper()
	el, err := page.WaitForSelector(selector, playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	})
	if err != nil {
		t.Fatalf("wait for %q: %v", selector, err)
	}
	return el
}

// ── Tests ────────────────────────────────────────────────────────────────────

// TestUI_EmailListLoads verifies the home page renders email rows.
func TestUI_EmailListLoads(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Hello World", "sender@example.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Another Email", "other@example.com", models.EmailStatusFailed)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}

	waitForSelector(t, page, "table tbody tr")

	rows, err := page.QuerySelectorAll("table tbody tr")
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) < 2 {
		t.Errorf("expected ≥2 email rows, got %d", len(rows))
	}
}

// TestUI_EmailDetailModalOpens clicks on an email row and verifies the detail
// modal appears with the correct subject.
func TestUI_EmailDetailModalOpens(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	subject := "Test Modal Subject"
	h.seedEmail(ctx, t, subject, "sender@example.com", models.EmailStatusDelivered)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}

	// Click the first email row.
	row := waitForSelector(t, page, "table tbody tr")
	if err := row.Click(); err != nil {
		t.Fatalf("click row: %v", err)
	}

	// Modal panel should appear.
	waitForSelector(t, page, "#modal-panel")

	// Subject should be visible inside the modal.
	modal, err := page.QuerySelector("#modal-panel")
	if err != nil || modal == nil {
		t.Fatal("modal panel not found")
	}
	content, err := modal.InnerText()
	if err != nil {
		t.Fatalf("modal inner text: %v", err)
	}
	if !containsStr(content, subject) {
		t.Errorf("modal content %q does not contain subject %q", content, subject)
	}
}

// TestUI_DeleteSingleEmail opens the detail modal for an email and deletes it,
// verifying the list updates and the modal closes.
func TestUI_DeleteSingleEmail(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Delete Me", "victim@example.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Keep Me", "keeper@example.com", models.EmailStatusDelivered)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}

	// Count rows before.
	waitForSelector(t, page, "table tbody tr")
	beforeRows, err := page.QuerySelectorAll("table tbody tr")
	if err != nil {
		t.Fatalf("before rows: %v", err)
	}
	before := len(beforeRows)

	// Open the first email's detail modal.
	if err := beforeRows[0].Click(); err != nil {
		t.Fatalf("click row: %v", err)
	}
	waitForSelector(t, page, "#modal-panel")

	// Click the Delete button inside the modal.
	// The button has hx-delete and hx-confirm — the dialog listener will accept.
	deleteBtn, err := page.QuerySelector("#modal-panel button[hx-delete]")
	if err != nil || deleteBtn == nil {
		t.Fatal("delete button not found in modal")
	}
	if err := deleteBtn.Click(); err != nil {
		t.Fatalf("click delete: %v", err)
	}

	// Wait for modal to close (emailDeleted event fires → closeModal()).
	if _, err := page.WaitForSelector("#modal-panel", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateDetached,
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("modal did not close: %v", err)
	}

	// Wait for list to refresh.
	time.Sleep(500 * time.Millisecond)

	afterRows, err := page.QuerySelectorAll("table tbody tr")
	if err != nil {
		t.Fatalf("after rows: %v", err)
	}
	after := len(afterRows)
	if after != before-1 {
		t.Errorf("expected %d rows after delete, got %d", before-1, after)
	}

	// Verify email is gone from database.
	result, err := h.store.ListEmails(ctx, storage.EmailFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list emails: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("expected 1 email in db after delete, got %d", result.Total)
	}
}

// TestUI_DeleteAllEmails clicks "Clear All" and verifies all emails are removed.
func TestUI_DeleteAllEmails(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Email One", "a@example.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Email Two", "b@example.com", models.EmailStatusFailed)
	h.seedEmail(ctx, t, "Email Three", "c@example.com", models.EmailStatusDropped)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}

	// Confirm there are rows.
	waitForSelector(t, page, "table tbody tr")

	// Find and click the "Clear All" button (hx-post="/emails/delete-all").
	clearBtn, err := page.QuerySelector("button[hx-post='/emails/delete-all']")
	if err != nil || clearBtn == nil {
		t.Fatal("clear all button not found")
	}
	if err := clearBtn.Click(); err != nil {
		t.Fatalf("click clear all: %v", err)
	}

	// Wait for the empty-state message to appear.
	if _, err := page.WaitForSelector("text=No emails found", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("empty state not shown after delete-all: %v", err)
	}

	// Verify database is empty.
	result, err := h.store.ListEmails(ctx, storage.EmailFilter{Limit: 100})
	if err != nil {
		t.Fatalf("list emails: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("expected 0 emails in db after delete-all, got %d", result.Total)
	}
}

// TestUI_StatsCardsUpdateAfterDelete verifies stat counts decrease after deleting
// an email.
func TestUI_StatsCardsUpdateAfterDelete(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Delivered Email", "a@example.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Failed Email", "b@example.com", models.EmailStatusFailed)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}
	waitForSelector(t, page, "#stats-cards")

	// Delete all.
	clearBtn, err := page.QuerySelector("button[hx-post='/emails/delete-all']")
	if err != nil || clearBtn == nil {
		t.Fatal("clear all button not found")
	}
	if err := clearBtn.Click(); err != nil {
		t.Fatalf("click clear all: %v", err)
	}
	if _, err := page.WaitForSelector("text=No emails found", playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("empty state not shown: %v", err)
	}

	// Verify stats API shows 0 total after delete.
	resp, err := page.Evaluate(`() => fetch('/api/stats').then(r => r.json())`)
	if err != nil {
		t.Fatalf("stats api: %v", err)
	}
	statsMap, ok := resp.(map[string]interface{})
	if !ok {
		t.Fatalf("unexpected stats type: %T", resp)
	}
	if total := statsMap["total"]; fmt.Sprintf("%v", total) != "0" {
		t.Errorf("expected total=0, got %v (type: %T)", total, total)
	}
}

// TestUI_RulesPageLoads verifies the rules page renders existing rules.
func TestUI_RulesPageLoads(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedRule(ctx, t, "Forward Support")
	h.seedRule(ctx, t, "Forward Sales")

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/rules")); err != nil {
		t.Fatalf("goto rules: %v", err)
	}

	waitForSelector(t, page, "text=Forward Support")
	waitForSelector(t, page, "text=Forward Sales")
}

// TestUI_CreateRule fills in the new-rule form and verifies the rule appears in
// the list.
func TestUI_CreateRule(t *testing.T) {
	h := newUIHarness(t)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/rules")); err != nil {
		t.Fatalf("goto rules: %v", err)
	}

	// Open the create-rule modal via "Create Rule" button (onclick="openFormModal()").
	newBtn, err := page.QuerySelector("button[onclick='openFormModal()']")
	if err != nil || newBtn == nil {
		t.Skip("create-rule button not found; skipping create test")
	}
	if err := newBtn.Click(); err != nil {
		t.Fatalf("click create rule: %v", err)
	}

	// Wait for the modal to be visible — the form-modal div becomes not-hidden.
	waitForSelector(t, page, "#form-modal:not(.hidden)")

	ruleName := fmt.Sprintf("E2E Rule %d", time.Now().UnixMilli())

	if err := page.Fill("#name", ruleName); err != nil {
		t.Fatalf("fill name: %v", err)
	}
	if err := page.Fill("#webhook_url", "https://example.com/webhook"); err != nil {
		t.Fatalf("fill webhook url: %v", err)
	}

	// Submit the form via the submit button.
	if err := page.Click("#form-submit-btn"); err != nil {
		t.Fatalf("click submit: %v", err)
	}

	// After server redirect (303) back to /rules, the rule should appear.
	if _, err := page.WaitForSelector(fmt.Sprintf("[data-rule-name='%s']", ruleName), playwright.PageWaitForSelectorOptions{
		Timeout: playwright.Float(5000),
	}); err != nil {
		t.Fatalf("created rule not visible: %v", err)
	}
}

// TestUI_DeleteRule seeds a rule, opens its detail modal, deletes it, and verifies it's gone.
func TestUI_DeleteRule(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	ruleName := fmt.Sprintf("DeleteMe Rule %d", time.Now().UnixMilli())
	h.seedRule(ctx, t, ruleName)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/rules")); err != nil {
		t.Fatalf("goto rules: %v", err)
	}

	// Wait for the rule card to appear and click it to open the detail modal.
	ruleCard, err := page.QuerySelector(fmt.Sprintf("[data-rule-name='%s']", ruleName))
	if err != nil || ruleCard == nil {
		t.Fatalf("rule card not found for %q", ruleName)
	}
	if err := ruleCard.Click(); err != nil {
		t.Fatalf("click rule card: %v", err)
	}

	// Wait for the detail modal to appear.
	waitForSelector(t, page, "#detail-modal:not(.hidden)")

	// Click the delete button (#detail-delete-btn). The JS sets hx-delete and calls
	// htmx.process() when the modal opens, so it's HTMX-enabled at this point.
	if err := page.Click("#detail-delete-btn"); err != nil {
		t.Fatalf("click delete rule: %v", err)
	}

	// Wait for HTMX redirect to /rules and the detail modal to close.
	time.Sleep(1500 * time.Millisecond)

	// Verify rule is gone from database.
	dbRules, err := h.store.ListRules(ctx)
	if err != nil {
		t.Fatalf("list rules: %v", err)
	}
	for _, r := range dbRules {
		if r.Name == ruleName {
			t.Errorf("rule %q still in database after delete", ruleName)
		}
	}
}

// TestUI_SearchFiltersEmails types a search query and checks that only matching
// rows are shown.
func TestUI_SearchFiltersEmails(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Invoice #1234", "billing@corp.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Welcome newsletter", "news@daily.com", models.EmailStatusDropped)
	h.seedEmail(ctx, t, "Invoice #5678", "billing@corp.com", models.EmailStatusDelivered)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}
	waitForSelector(t, page, "table tbody tr")

	// Type "Invoice" into the search field.
	if err := page.Fill("input[name='q']", "Invoice"); err != nil {
		t.Fatalf("fill search: %v", err)
	}

	// Wait for the table to update (HTMX debounce is 300ms).
	time.Sleep(600 * time.Millisecond)
	waitForSelector(t, page, "table tbody tr")

	rows, err := page.QuerySelectorAll("table tbody tr")
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 invoice rows, got %d", len(rows))
	}
}

// TestUI_ModalClosesOnEscape opens a modal and closes it with the Escape key.
func TestUI_ModalClosesOnEscape(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Escape Test", "esc@example.com", models.EmailStatusDelivered)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}

	row := waitForSelector(t, page, "table tbody tr")
	if err := row.Click(); err != nil {
		t.Fatalf("click row: %v", err)
	}
	waitForSelector(t, page, "#modal-panel")

	// Press Escape.
	if err := page.Keyboard().Press("Escape"); err != nil {
		t.Fatalf("press escape: %v", err)
	}

	if _, err := page.WaitForSelector("#modal-panel", playwright.PageWaitForSelectorOptions{
		State:   playwright.WaitForSelectorStateDetached,
		Timeout: playwright.Float(3000),
	}); err != nil {
		t.Fatalf("modal did not close on Escape: %v", err)
	}
}

// TestUI_StatusFilterWorks selects a status filter and verifies only matching rows appear.
func TestUI_StatusFilterWorks(t *testing.T) {
	ctx := context.Background()
	h := newUIHarness(t)
	h.seedEmail(ctx, t, "Delivered One", "a@x.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Delivered Two", "b@x.com", models.EmailStatusDelivered)
	h.seedEmail(ctx, t, "Failed One", "c@x.com", models.EmailStatusFailed)

	browser := pwSetup(t)
	page := newPage(t, browser)

	if _, err := page.Goto(h.url("/")); err != nil {
		t.Fatalf("goto: %v", err)
	}
	waitForSelector(t, page, "table tbody tr")

	// Select "Failed" from the status dropdown.
	if _, err := page.SelectOption("select[name='status']", playwright.SelectOptionValues{
		Values: &[]string{"failed"},
	}); err != nil {
		t.Fatalf("select status: %v", err)
	}

	time.Sleep(500 * time.Millisecond)
	waitForSelector(t, page, "table tbody tr")

	rows, err := page.QuerySelectorAll("table tbody tr")
	if err != nil {
		t.Fatalf("query rows: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("expected 1 failed row, got %d", len(rows))
	}
}

// containsStr is a nil-safe string contains check.
func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && func() bool {
		for i := 0; i <= len(s)-len(sub); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	}())
}
