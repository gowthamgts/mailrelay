package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gowthamgts/mailrelay/internal/models"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type EmailFilter struct {
	Status string
	Search string
	Limit  int
	Offset int
}

type EmailListResult struct {
	Emails []models.EmailRecord
	Total  int
}

type Stats struct {
	Total          int `json:"total"`
	Delivered      int `json:"delivered"`
	Dropped        int `json:"dropped"`
	Failed         int `json:"failed"`
	PartialFailure int `json:"partial_failure"`
	Rejected       int `json:"rejected"`
}

func Open(dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	db.SetMaxOpenConns(1)

	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS emails (
		id               TEXT PRIMARY KEY,
		received_at      DATETIME NOT NULL,
		envelope_from    TEXT NOT NULL,
		envelope_to      TEXT NOT NULL,
		subject          TEXT NOT NULL DEFAULT '',
		status           TEXT NOT NULL DEFAULT 'received',
		rejection_reason TEXT NOT NULL DEFAULT '',
		parsed_email     TEXT NOT NULL,
		auth_result      TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_emails_received_at ON emails(received_at);
	CREATE INDEX IF NOT EXISTS idx_emails_status ON emails(status);

	CREATE TABLE IF NOT EXISTS deliveries (
		id            TEXT PRIMARY KEY,
		email_id      TEXT NOT NULL REFERENCES emails(id) ON DELETE CASCADE,
		rule_name     TEXT NOT NULL,
		status        TEXT NOT NULL,
		status_code   INTEGER NOT NULL DEFAULT 0,
		error_message TEXT NOT NULL DEFAULT '',
		attempts      INTEGER NOT NULL DEFAULT 0,
		created_at    DATETIME NOT NULL,
		updated_at    DATETIME NOT NULL
	);

	CREATE INDEX IF NOT EXISTS idx_deliveries_email_id ON deliveries(email_id);

	CREATE TABLE IF NOT EXISTS rules (
		id             TEXT PRIMARY KEY,
		name           TEXT NOT NULL UNIQUE,
		enabled        INTEGER NOT NULL DEFAULT 1,
		match_config   TEXT NOT NULL DEFAULT '{}',
		webhook_config TEXT NOT NULL DEFAULT '{}',
		position       INTEGER NOT NULL DEFAULT 0,
		created_at     DATETIME NOT NULL,
		updated_at     DATETIME NOT NULL
	);

	CREATE TABLE IF NOT EXISTS settings (
		id          INTEGER PRIMARY KEY CHECK (id = 1),
		config_json TEXT NOT NULL,
		updated_at  DATETIME NOT NULL
	);
	`
	_, err := db.Exec(schema)
	if err != nil {
		return err
	}

	// Add rejection_reason column to existing databases.
	db.Exec(`ALTER TABLE emails ADD COLUMN rejection_reason TEXT NOT NULL DEFAULT ''`)
	// Add response_body column to existing databases.
	db.Exec(`ALTER TABLE deliveries ADD COLUMN response_body TEXT NOT NULL DEFAULT ''`)
	// Add header From/To columns to distinguish from envelope addresses.
	db.Exec(`ALTER TABLE emails ADD COLUMN header_from TEXT NOT NULL DEFAULT ''`)
	db.Exec(`ALTER TABLE emails ADD COLUMN header_to TEXT NOT NULL DEFAULT '[]'`)

	return nil
}

func (s *Store) SaveEmail(ctx context.Context, record *models.EmailRecord) error {
	if record.ID == "" {
		record.ID = uuid.New().String()
	}
	if record.ReceivedAt.IsZero() {
		record.ReceivedAt = time.Now().UTC()
	}

	envelopeTo, _ := json.Marshal(record.EnvelopeTo)
	headerTo, _ := json.Marshal(record.HeaderTo)
	parsedEmail, _ := json.Marshal(record.ParsedEmail)
	var authResult []byte
	if record.AuthResult != nil {
		authResult, _ = json.Marshal(record.AuthResult)
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO emails (id, received_at, envelope_from, envelope_to, header_from, header_to, subject, status, rejection_reason, parsed_email, auth_result)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.ReceivedAt, record.EnvelopeFrom, string(envelopeTo),
		record.HeaderFrom, string(headerTo),
		record.Subject, string(record.Status), record.RejectionReason, string(parsedEmail),
		nullString(authResult),
	)
	return err
}

func (s *Store) UpdateEmailStatus(ctx context.Context, id string, status models.EmailStatus) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE emails SET status = ? WHERE id = ?`,
		string(status), id,
	)
	return err
}

func (s *Store) GetEmail(ctx context.Context, id string) (*models.EmailRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, received_at, envelope_from, envelope_to, header_from, header_to, subject, status, rejection_reason, parsed_email, auth_result
		 FROM emails WHERE id = ?`, id,
	)
	return scanEmail(row)
}

func (s *Store) ListEmails(ctx context.Context, filter EmailFilter) (*EmailListResult, error) {
	if filter.Limit <= 0 {
		filter.Limit = 50
	}

	var where []string
	var args []any

	if filter.Status != "" {
		where = append(where, "status = ?")
		args = append(args, filter.Status)
	}
	if filter.Search != "" {
		where = append(where, "(subject LIKE ? OR envelope_from LIKE ? OR envelope_to LIKE ? OR header_from LIKE ? OR header_to LIKE ?)")
		search := "%" + filter.Search + "%"
		args = append(args, search, search, search, search, search)
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM emails "+whereClause, args...).Scan(&total)
	if err != nil {
		return nil, fmt.Errorf("counting emails: %w", err)
	}

	query := fmt.Sprintf(
		"SELECT id, received_at, envelope_from, envelope_to, header_from, header_to, subject, status, rejection_reason, parsed_email, auth_result FROM emails %s ORDER BY received_at DESC LIMIT ? OFFSET ?",
		whereClause,
	)
	args = append(args, filter.Limit, filter.Offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing emails: %w", err)
	}
	defer rows.Close()

	var emails []models.EmailRecord
	for rows.Next() {
		rec, err := scanEmailRow(rows)
		if err != nil {
			return nil, err
		}
		rec.ParsedEmail = nil
		emails = append(emails, *rec)
	}

	return &EmailListResult{Emails: emails, Total: total}, nil
}

func (s *Store) SaveDelivery(ctx context.Context, d *models.DeliveryRecord) error {
	if d.ID == "" {
		d.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if d.CreatedAt.IsZero() {
		d.CreatedAt = now
	}
	d.UpdatedAt = now

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO deliveries (id, email_id, rule_name, status, status_code, error_message, response_body, attempts, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.EmailID, d.RuleName, d.Status, d.StatusCode,
		d.ErrorMessage, d.ResponseBody, d.Attempts, d.CreatedAt, d.UpdatedAt,
	)
	return err
}

func (s *Store) GetDeliveriesForEmail(ctx context.Context, emailID string) ([]models.DeliveryRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, email_id, rule_name, status, status_code, error_message, response_body, attempts, created_at, updated_at
		 FROM deliveries WHERE email_id = ? ORDER BY created_at DESC`, emailID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deliveries []models.DeliveryRecord
	for rows.Next() {
		var d models.DeliveryRecord
		if err := rows.Scan(&d.ID, &d.EmailID, &d.RuleName, &d.Status, &d.StatusCode,
			&d.ErrorMessage, &d.ResponseBody, &d.Attempts, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, d)
	}
	return deliveries, nil
}

func (s *Store) Stats(ctx context.Context) (*Stats, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM emails GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	st := &Stats{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		st.Total += count
		switch models.EmailStatus(status) {
		case models.EmailStatusDelivered:
			st.Delivered = count
		case models.EmailStatusDropped:
			st.Dropped = count
		case models.EmailStatusFailed:
			st.Failed = count
		case models.EmailStatusPartialFailure:
			st.PartialFailure = count
		case models.EmailStatusRejected:
			st.Rejected = count
		}
	}
	return st, nil
}

func (s *Store) DeleteEmail(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM emails WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteAllEmails(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM emails`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) AllEmailIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM emails`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Store) GetExpiredEmailIDs(ctx context.Context, retentionDays int) ([]string, error) {
	if retentionDays <= 0 {
		return nil, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM emails WHERE received_at < ?`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Store) Cleanup(ctx context.Context, retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(retentionDays) * 24 * time.Hour)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM emails WHERE received_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- Rule CRUD ---

func (s *Store) SaveRule(ctx context.Context, rule *models.RuleRecord) error {
	if rule.ID == "" {
		rule.ID = uuid.New().String()
	}
	now := time.Now().UTC()
	if rule.CreatedAt.IsZero() {
		rule.CreatedAt = now
	}
	rule.UpdatedAt = now

	matchJSON, _ := json.Marshal(rule.Match)
	webhookJSON, _ := json.Marshal(rule.Webhook)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO rules (id, name, enabled, match_config, webhook_config, position, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		rule.ID, rule.Name, boolToInt(rule.Enabled), string(matchJSON), string(webhookJSON),
		rule.Position, rule.CreatedAt, rule.UpdatedAt,
	)
	return err
}

func (s *Store) UpdateRule(ctx context.Context, rule *models.RuleRecord) error {
	rule.UpdatedAt = time.Now().UTC()
	matchJSON, _ := json.Marshal(rule.Match)
	webhookJSON, _ := json.Marshal(rule.Webhook)

	_, err := s.db.ExecContext(ctx,
		`UPDATE rules SET name = ?, enabled = ?, match_config = ?, webhook_config = ?, position = ?, updated_at = ?
		 WHERE id = ?`,
		rule.Name, boolToInt(rule.Enabled), string(matchJSON), string(webhookJSON),
		rule.Position, rule.UpdatedAt, rule.ID,
	)
	return err
}

func (s *Store) GetRule(ctx context.Context, id string) (*models.RuleRecord, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, enabled, match_config, webhook_config, position, created_at, updated_at
		 FROM rules WHERE id = ?`, id,
	)
	return scanRule(row)
}

func (s *Store) ListRules(ctx context.Context) ([]models.RuleRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, enabled, match_config, webhook_config, position, created_at, updated_at
		 FROM rules ORDER BY position, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []models.RuleRecord
	for rows.Next() {
		r, err := scanRuleRow(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, *r)
	}
	return rules, nil
}

func (s *Store) ListEnabledRules(ctx context.Context) ([]models.RuleRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, enabled, match_config, webhook_config, position, created_at, updated_at
		 FROM rules WHERE enabled = 1 ORDER BY position, created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rules []models.RuleRecord
	for rows.Next() {
		r, err := scanRuleRow(rows)
		if err != nil {
			return nil, err
		}
		rules = append(rules, *r)
	}
	return rules, nil
}

func (s *Store) DeleteRule(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM rules WHERE id = ?`, id)
	return err
}

func (s *Store) NextRulePosition(ctx context.Context) (int, error) {
	var pos sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(position) FROM rules`).Scan(&pos)
	if err != nil {
		return 0, err
	}
	if !pos.Valid {
		return 0, nil
	}
	return int(pos.Int64) + 1, nil
}

func scanRule(row *sql.Row) (*models.RuleRecord, error) {
	var r models.RuleRecord
	var matchJSON, webhookJSON string
	var enabled int

	err := row.Scan(&r.ID, &r.Name, &enabled, &matchJSON, &webhookJSON,
		&r.Position, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}

	r.Enabled = enabled != 0
	json.Unmarshal([]byte(matchJSON), &r.Match)
	json.Unmarshal([]byte(webhookJSON), &r.Webhook)
	return &r, nil
}

func scanRuleRow(rows *sql.Rows) (*models.RuleRecord, error) {
	var r models.RuleRecord
	var matchJSON, webhookJSON string
	var enabled int

	err := rows.Scan(&r.ID, &r.Name, &enabled, &matchJSON, &webhookJSON,
		&r.Position, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}

	r.Enabled = enabled != 0
	json.Unmarshal([]byte(matchJSON), &r.Match)
	json.Unmarshal([]byte(webhookJSON), &r.Webhook)
	return &r, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *Store) Close() error {
	return s.db.Close()
}

type scanner interface {
	Scan(dest ...any) error
}

func populateEmailRecord(rec *models.EmailRecord, envelopeTo, headerTo, parsedEmail, status string, authResult sql.NullString) {
	rec.Status = models.EmailStatus(status)
	json.Unmarshal([]byte(envelopeTo), &rec.EnvelopeTo)
	json.Unmarshal([]byte(headerTo), &rec.HeaderTo)

	var pe models.ParsedEmail
	if err := json.Unmarshal([]byte(parsedEmail), &pe); err == nil {
		rec.ParsedEmail = &pe
	}

	if authResult.Valid {
		var ar models.AuthResult
		if err := json.Unmarshal([]byte(authResult.String), &ar); err == nil {
			rec.AuthResult = &ar
		}
	}
}

func scanEmail(row *sql.Row) (*models.EmailRecord, error) {
	var rec models.EmailRecord
	var envelopeTo, headerTo, parsedEmail string
	var authResult sql.NullString
	var status string

	err := row.Scan(&rec.ID, &rec.ReceivedAt, &rec.EnvelopeFrom, &envelopeTo,
		&rec.HeaderFrom, &headerTo,
		&rec.Subject, &status, &rec.RejectionReason, &parsedEmail, &authResult)
	if err != nil {
		return nil, err
	}

	populateEmailRecord(&rec, envelopeTo, headerTo, parsedEmail, status, authResult)
	return &rec, nil
}

func scanEmailRow(rows *sql.Rows) (*models.EmailRecord, error) {
	var rec models.EmailRecord
	var envelopeTo, headerTo, parsedEmail string
	var authResult sql.NullString
	var status string

	err := rows.Scan(&rec.ID, &rec.ReceivedAt, &rec.EnvelopeFrom, &envelopeTo,
		&rec.HeaderFrom, &headerTo,
		&rec.Subject, &status, &rec.RejectionReason, &parsedEmail, &authResult)
	if err != nil {
		return nil, err
	}

	populateEmailRecord(&rec, envelopeTo, headerTo, parsedEmail, status, authResult)
	return &rec, nil
}

// SaveSettings persists the full AppConfig as JSON.
func (s *Store) SaveSettings(ctx context.Context, cfg *models.AppConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshalling settings: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO settings (id, config_json, updated_at) VALUES (1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET config_json = excluded.config_json, updated_at = excluded.updated_at`,
		string(data), time.Now().UTC(),
	)
	return err
}

// LoadSettings loads the saved AppConfig from the database.
// Returns nil, nil if no settings have been saved yet.
func (s *Store) LoadSettings(ctx context.Context) (*models.AppConfig, error) {
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT config_json FROM settings WHERE id = 1`).Scan(&data)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg models.AppConfig
	if err := json.Unmarshal([]byte(data), &cfg); err != nil {
		return nil, fmt.Errorf("unmarshalling settings: %w", err)
	}
	return &cfg, nil
}

func nullString(b []byte) sql.NullString {
	if b == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}
