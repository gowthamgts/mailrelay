package storage

import (
	"context"
	"testing"
	"time"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open() error: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func sampleEmail(subject string) *models.EmailRecord {
	return &models.EmailRecord{
		EnvelopeFrom: "sender@example.com",
		EnvelopeTo:   []string{"recipient@example.com"},
		Subject:      subject,
		Status:       models.EmailStatusReceived,
		ParsedEmail: &models.ParsedEmail{
			From:    "sender@example.com",
			Subject: subject,
		},
	}
}

func TestSaveAndGetEmail(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("Hello")
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}
	if rec.ID == "" {
		t.Fatal("expected ID to be set after SaveEmail")
	}

	got, err := s.GetEmail(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetEmail() error: %v", err)
	}
	if got.ID != rec.ID {
		t.Errorf("ID = %q, want %q", got.ID, rec.ID)
	}
	if got.Subject != "Hello" {
		t.Errorf("Subject = %q, want Hello", got.Subject)
	}
	if got.EnvelopeFrom != "sender@example.com" {
		t.Errorf("EnvelopeFrom = %q, want sender@example.com", got.EnvelopeFrom)
	}
	if len(got.EnvelopeTo) != 1 || got.EnvelopeTo[0] != "recipient@example.com" {
		t.Errorf("EnvelopeTo = %v, want [recipient@example.com]", got.EnvelopeTo)
	}
}

func TestSaveEmail_GeneratesIDAndTimestamp(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := &models.EmailRecord{
		EnvelopeFrom: "a@b.com",
		EnvelopeTo:   []string{"c@d.com"},
		Subject:      "Auto ID",
		Status:       models.EmailStatusReceived,
		ParsedEmail:  &models.ParsedEmail{},
	}
	before := time.Now().UTC().Add(-time.Second)
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}
	after := time.Now().UTC().Add(time.Second)

	if rec.ID == "" {
		t.Error("expected non-empty ID")
	}
	if rec.ReceivedAt.Before(before) || rec.ReceivedAt.After(after) {
		t.Errorf("ReceivedAt %v not in expected range [%v, %v]", rec.ReceivedAt, before, after)
	}
}

func TestSaveEmail_PreservesProvidedID(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("Custom ID")
	rec.ID = "my-custom-id"
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}
	if rec.ID != "my-custom-id" {
		t.Errorf("ID = %q, want my-custom-id", rec.ID)
	}
}

func TestSaveEmail_WithAuthResult(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("Auth test")
	rec.AuthResult = &models.AuthResult{
		SPF:  models.AuthPass,
		DKIM: models.AuthFail,
	}
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}

	got, err := s.GetEmail(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetEmail() error: %v", err)
	}
	if got.AuthResult == nil {
		t.Fatal("expected non-nil AuthResult")
	}
	if got.AuthResult.SPF != models.AuthPass {
		t.Errorf("SPF = %q, want pass", got.AuthResult.SPF)
	}
	if got.AuthResult.DKIM != models.AuthFail {
		t.Errorf("DKIM = %q, want fail", got.AuthResult.DKIM)
	}
}

func TestGetEmail_NotFound(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.GetEmail(ctx, "nonexistent-id")
	if err == nil {
		t.Fatal("expected error for nonexistent email")
	}
}

func TestUpdateEmailStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("Status test")
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}

	if err := s.UpdateEmailStatus(ctx, rec.ID, models.EmailStatusDelivered); err != nil {
		t.Fatalf("UpdateEmailStatus() error: %v", err)
	}

	got, err := s.GetEmail(ctx, rec.ID)
	if err != nil {
		t.Fatalf("GetEmail() error: %v", err)
	}
	if got.Status != models.EmailStatusDelivered {
		t.Errorf("Status = %q, want delivered", got.Status)
	}
}

func TestListEmails_NoFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := s.SaveEmail(ctx, sampleEmail("email")); err != nil {
			t.Fatalf("SaveEmail() error: %v", err)
		}
	}

	result, err := s.ListEmails(ctx, EmailFilter{})
	if err != nil {
		t.Fatalf("ListEmails() error: %v", err)
	}
	if result.Total != 3 {
		t.Errorf("Total = %d, want 3", result.Total)
	}
	if len(result.Emails) != 3 {
		t.Errorf("len(Emails) = %d, want 3", len(result.Emails))
	}
	// ParsedEmail should be nil in list results
	for _, e := range result.Emails {
		if e.ParsedEmail != nil {
			t.Error("expected ParsedEmail to be nil in list results")
		}
	}
}

func TestListEmails_StatusFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec1 := sampleEmail("delivered")
	if err := s.SaveEmail(ctx, rec1); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}
	s.UpdateEmailStatus(ctx, rec1.ID, models.EmailStatusDelivered)

	if err := s.SaveEmail(ctx, sampleEmail("received")); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}

	result, err := s.ListEmails(ctx, EmailFilter{Status: "delivered"})
	if err != nil {
		t.Fatalf("ListEmails() error: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
}

func TestListEmails_SearchFilter(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.SaveEmail(ctx, sampleEmail("unique-subject"))
	s.SaveEmail(ctx, sampleEmail("another-subject"))

	result, err := s.ListEmails(ctx, EmailFilter{Search: "unique"})
	if err != nil {
		t.Fatalf("ListEmails() error: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
	if result.Emails[0].Subject != "unique-subject" {
		t.Errorf("Subject = %q, want unique-subject", result.Emails[0].Subject)
	}
}

func TestListEmails_Pagination(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		s.SaveEmail(ctx, sampleEmail("email"))
	}

	result, err := s.ListEmails(ctx, EmailFilter{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("ListEmails() error: %v", err)
	}
	if result.Total != 5 {
		t.Errorf("Total = %d, want 5", result.Total)
	}
	if len(result.Emails) != 2 {
		t.Errorf("len(Emails) = %d, want 2", len(result.Emails))
	}

	result2, err := s.ListEmails(ctx, EmailFilter{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("ListEmails() error: %v", err)
	}
	if len(result2.Emails) != 2 {
		t.Errorf("len(Emails) = %d, want 2", len(result2.Emails))
	}
}

func TestDeleteEmail(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("delete me")
	s.SaveEmail(ctx, rec)

	if err := s.DeleteEmail(ctx, rec.ID); err != nil {
		t.Fatalf("DeleteEmail() error: %v", err)
	}

	_, err := s.GetEmail(ctx, rec.ID)
	if err == nil {
		t.Fatal("expected error after deletion")
	}
}

func TestDeleteAllEmails(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		s.SaveEmail(ctx, sampleEmail("email"))
	}

	n, err := s.DeleteAllEmails(ctx)
	if err != nil {
		t.Fatalf("DeleteAllEmails() error: %v", err)
	}
	if n != 3 {
		t.Errorf("deleted %d, want 3", n)
	}

	result, err := s.ListEmails(ctx, EmailFilter{})
	if err != nil {
		t.Fatalf("ListEmails() error: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0 after delete all", result.Total)
	}
}

func TestAllEmailIDs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec1 := sampleEmail("a")
	rec2 := sampleEmail("b")
	s.SaveEmail(ctx, rec1)
	s.SaveEmail(ctx, rec2)

	ids, err := s.AllEmailIDs(ctx)
	if err != nil {
		t.Fatalf("AllEmailIDs() error: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("len(ids) = %d, want 2", len(ids))
	}
}

func TestGetExpiredEmailIDs_ZeroRetention(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.SaveEmail(ctx, sampleEmail("old"))

	ids, err := s.GetExpiredEmailIDs(ctx, 0)
	if err != nil {
		t.Fatalf("GetExpiredEmailIDs() error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("expected 0 expired IDs with retention=0, got %d", len(ids))
	}
}

func TestGetExpiredEmailIDs_WithExpiredEmail(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// Insert an email with a past received_at.
	rec := sampleEmail("old email")
	rec.ID = "old-email"
	rec.ReceivedAt = time.Now().UTC().Add(-48 * time.Hour)
	if err := s.SaveEmail(ctx, rec); err != nil {
		t.Fatalf("SaveEmail() error: %v", err)
	}

	// Insert a recent email.
	s.SaveEmail(ctx, sampleEmail("new email"))

	ids, err := s.GetExpiredEmailIDs(ctx, 1) // 1 day retention
	if err != nil {
		t.Fatalf("GetExpiredEmailIDs() error: %v", err)
	}
	if len(ids) != 1 || ids[0] != "old-email" {
		t.Errorf("expired IDs = %v, want [old-email]", ids)
	}
}

func TestCleanup(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("cleanup test")
	rec.ReceivedAt = time.Now().UTC().Add(-48 * time.Hour)
	s.SaveEmail(ctx, rec)
	s.SaveEmail(ctx, sampleEmail("recent"))

	n, err := s.Cleanup(ctx, 1)
	if err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
	if n != 1 {
		t.Errorf("Cleanup() deleted %d, want 1", n)
	}
}

func TestCleanup_ZeroRetention(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rec := sampleEmail("no cleanup")
	rec.ReceivedAt = time.Now().UTC().Add(-48 * time.Hour)
	s.SaveEmail(ctx, rec)

	n, err := s.Cleanup(ctx, 0)
	if err != nil {
		t.Fatalf("Cleanup() error: %v", err)
	}
	if n != 0 {
		t.Errorf("Cleanup(0) deleted %d, want 0", n)
	}
}

func TestSaveAndGetDeliveries(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	email := sampleEmail("delivery test")
	s.SaveEmail(ctx, email)

	d := &models.DeliveryRecord{
		EmailID:  email.ID,
		RuleName: "my-rule",
		Status:   "success",
		Attempts: 1,
	}
	if err := s.SaveDelivery(ctx, d); err != nil {
		t.Fatalf("SaveDelivery() error: %v", err)
	}
	if d.ID == "" {
		t.Fatal("expected delivery ID to be set")
	}

	deliveries, err := s.GetDeliveriesForEmail(ctx, email.ID)
	if err != nil {
		t.Fatalf("GetDeliveriesForEmail() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("len(deliveries) = %d, want 1", len(deliveries))
	}
	if deliveries[0].RuleName != "my-rule" {
		t.Errorf("RuleName = %q, want my-rule", deliveries[0].RuleName)
	}
	if deliveries[0].Status != "success" {
		t.Errorf("Status = %q, want success", deliveries[0].Status)
	}
}

func TestGetDeliveriesForEmail_Empty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	deliveries, err := s.GetDeliveriesForEmail(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("GetDeliveriesForEmail() error: %v", err)
	}
	if len(deliveries) != 0 {
		t.Errorf("expected 0 deliveries, got %d", len(deliveries))
	}
}

func TestStats(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	statuses := []models.EmailStatus{
		models.EmailStatusDelivered,
		models.EmailStatusDelivered,
		models.EmailStatusFailed,
		models.EmailStatusDropped,
		models.EmailStatusPartialFailure,
		models.EmailStatusRejected,
	}

	for _, status := range statuses {
		rec := sampleEmail("stats test")
		s.SaveEmail(ctx, rec)
		s.UpdateEmailStatus(ctx, rec.ID, status)
	}

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if st.Total != 6 {
		t.Errorf("Total = %d, want 6", st.Total)
	}
	if st.Delivered != 2 {
		t.Errorf("Delivered = %d, want 2", st.Delivered)
	}
	if st.Failed != 1 {
		t.Errorf("Failed = %d, want 1", st.Failed)
	}
	if st.Dropped != 1 {
		t.Errorf("Dropped = %d, want 1", st.Dropped)
	}
	if st.PartialFailure != 1 {
		t.Errorf("PartialFailure = %d, want 1", st.PartialFailure)
	}
	if st.Rejected != 1 {
		t.Errorf("Rejected = %d, want 1", st.Rejected)
	}
}

func TestStats_Empty(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats() error: %v", err)
	}
	if st.Total != 0 {
		t.Errorf("Total = %d, want 0", st.Total)
	}
}

func TestSaveAndGetRule(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rule := &models.RuleRecord{
		Name:    "test-rule",
		Enabled: true,
		Match:   models.MatcherConfig{ToEmail: "user@example.com"},
		Webhook: models.WebhookConfig{URL: "https://example.com/hook", Method: "POST"},
	}
	if err := s.SaveRule(ctx, rule); err != nil {
		t.Fatalf("SaveRule() error: %v", err)
	}
	if rule.ID == "" {
		t.Fatal("expected rule ID to be set")
	}

	got, err := s.GetRule(ctx, rule.ID)
	if err != nil {
		t.Fatalf("GetRule() error: %v", err)
	}
	if got.Name != "test-rule" {
		t.Errorf("Name = %q, want test-rule", got.Name)
	}
	if !got.Enabled {
		t.Error("expected Enabled = true")
	}
	if got.Match.ToEmail != "user@example.com" {
		t.Errorf("Match.ToEmail = %q, want user@example.com", got.Match.ToEmail)
	}
	if got.Webhook.URL != "https://example.com/hook" {
		t.Errorf("Webhook.URL = %q, want https://example.com/hook", got.Webhook.URL)
	}
}

func TestUpdateRule(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rule := &models.RuleRecord{
		Name:    "original",
		Enabled: true,
		Webhook: models.WebhookConfig{URL: "https://example.com", Method: "POST"},
	}
	s.SaveRule(ctx, rule)

	rule.Name = "updated"
	rule.Enabled = false
	if err := s.UpdateRule(ctx, rule); err != nil {
		t.Fatalf("UpdateRule() error: %v", err)
	}

	got, err := s.GetRule(ctx, rule.ID)
	if err != nil {
		t.Fatalf("GetRule() error: %v", err)
	}
	if got.Name != "updated" {
		t.Errorf("Name = %q, want updated", got.Name)
	}
	if got.Enabled {
		t.Error("expected Enabled = false")
	}
}

func TestListRules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	for _, name := range []string{"rule-a", "rule-b", "rule-c"} {
		s.SaveRule(ctx, &models.RuleRecord{
			Name:    name,
			Enabled: true,
			Webhook: models.WebhookConfig{Method: "POST"},
		})
	}

	rules, err := s.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules() error: %v", err)
	}
	if len(rules) != 3 {
		t.Errorf("len(rules) = %d, want 3", len(rules))
	}
}

func TestListEnabledRules(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	r1 := &models.RuleRecord{Name: "enabled", Enabled: true, Webhook: models.WebhookConfig{Method: "POST"}}
	r2 := &models.RuleRecord{Name: "disabled", Enabled: false, Webhook: models.WebhookConfig{Method: "POST"}}
	s.SaveRule(ctx, r1)
	s.SaveRule(ctx, r2)

	rules, err := s.ListEnabledRules(ctx)
	if err != nil {
		t.Fatalf("ListEnabledRules() error: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("len(rules) = %d, want 1", len(rules))
	}
	if rules[0].Name != "enabled" {
		t.Errorf("Name = %q, want enabled", rules[0].Name)
	}
}

func TestDeleteRule(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	rule := &models.RuleRecord{
		Name:    "delete-me",
		Enabled: true,
		Webhook: models.WebhookConfig{Method: "POST"},
	}
	s.SaveRule(ctx, rule)

	if err := s.DeleteRule(ctx, rule.ID); err != nil {
		t.Fatalf("DeleteRule() error: %v", err)
	}

	rules, err := s.ListRules(ctx)
	if err != nil {
		t.Fatalf("ListRules() error: %v", err)
	}
	if len(rules) != 0 {
		t.Errorf("expected 0 rules after delete, got %d", len(rules))
	}
}

func TestNextRulePosition(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	pos, err := s.NextRulePosition(ctx)
	if err != nil {
		t.Fatalf("NextRulePosition() error: %v", err)
	}
	if pos != 0 {
		t.Errorf("NextRulePosition() = %d, want 0 for empty table", pos)
	}

	s.SaveRule(ctx, &models.RuleRecord{
		Name:     "r1",
		Position: 0,
		Webhook:  models.WebhookConfig{Method: "POST"},
	})
	s.SaveRule(ctx, &models.RuleRecord{
		Name:     "r2",
		Position: 5,
		Webhook:  models.WebhookConfig{Method: "POST"},
	})

	pos, err = s.NextRulePosition(ctx)
	if err != nil {
		t.Fatalf("NextRulePosition() error: %v", err)
	}
	if pos != 6 {
		t.Errorf("NextRulePosition() = %d, want 6", pos)
	}
}

func TestSaveAndLoadSettings(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// No settings yet — should return nil.
	cfg, err := s.LoadSettings(ctx)
	if err != nil {
		t.Fatalf("LoadSettings() error: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil settings before any save")
	}

	toSave := &models.AppConfig{
		LogLevel: "debug",
		SMTP: models.SMTPConfig{
			Addr:   ":2525",
			Domain: "example.com",
		},
	}
	if err := s.SaveSettings(ctx, toSave); err != nil {
		t.Fatalf("SaveSettings() error: %v", err)
	}

	loaded, err := s.LoadSettings(ctx)
	if err != nil {
		t.Fatalf("LoadSettings() error: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil loaded settings")
	}
	if loaded.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", loaded.LogLevel)
	}
	if loaded.SMTP.Addr != ":2525" {
		t.Errorf("SMTP.Addr = %q, want :2525", loaded.SMTP.Addr)
	}
}

func TestSaveSettings_Upsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	s.SaveSettings(ctx, &models.AppConfig{LogLevel: "info"})
	s.SaveSettings(ctx, &models.AppConfig{LogLevel: "warn"})

	loaded, err := s.LoadSettings(ctx)
	if err != nil {
		t.Fatalf("LoadSettings() error: %v", err)
	}
	if loaded.LogLevel != "warn" {
		t.Errorf("LogLevel = %q, want warn after upsert", loaded.LogLevel)
	}
}

func TestSaveDelivery_WithResponseBody(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	email := sampleEmail("response body test")
	s.SaveEmail(ctx, email)

	d := &models.DeliveryRecord{
		EmailID:      email.ID,
		RuleName:     "rule",
		Status:       "success",
		StatusCode:   200,
		ResponseBody: `{"ok":true}`,
		Attempts:     1,
	}
	if err := s.SaveDelivery(ctx, d); err != nil {
		t.Fatalf("SaveDelivery() error: %v", err)
	}

	deliveries, err := s.GetDeliveriesForEmail(ctx, email.ID)
	if err != nil {
		t.Fatalf("GetDeliveriesForEmail() error: %v", err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("len(deliveries) = %d, want 1", len(deliveries))
	}
	if deliveries[0].ResponseBody != `{"ok":true}` {
		t.Errorf("ResponseBody = %q, want {\"ok\":true}", deliveries[0].ResponseBody)
	}
	if deliveries[0].StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", deliveries[0].StatusCode)
	}
}

func TestBoolToInt(t *testing.T) {
	if boolToInt(true) != 1 {
		t.Error("boolToInt(true) should be 1")
	}
	if boolToInt(false) != 0 {
		t.Error("boolToInt(false) should be 0")
	}
}

func TestNullString(t *testing.T) {
	ns := nullString(nil)
	if ns.Valid {
		t.Error("expected invalid NullString for nil input")
	}

	ns = nullString([]byte("hello"))
	if !ns.Valid {
		t.Error("expected valid NullString for non-nil input")
	}
	if ns.String != "hello" {
		t.Errorf("String = %q, want hello", ns.String)
	}
}
