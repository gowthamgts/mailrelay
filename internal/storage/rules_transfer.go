package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/gowthamgts/mailrelay/internal/rules"
)

// ImportRules validates every rule before applying the entire file atomically.
// Merge preserves the identity of existing rules, matching exact names.
func (s *Store) ImportRules(ctx context.Context, items []rules.TransferRule, replace bool) error {
	if err := rules.ValidateImport(items); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if replace {
		if _, err := tx.ExecContext(ctx, `DELETE FROM rules`); err != nil {
			return err
		}
	}
	for _, r := range items {
		match, _ := json.Marshal(r.Match)
		wh, _ := json.Marshal(r.Webhook)
		now := time.Now().UTC()
		_, err := tx.ExecContext(ctx, `INSERT INTO rules (id,name,enabled,match_config,webhook_config,position,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)
   ON CONFLICT(name) DO UPDATE SET enabled=excluded.enabled, match_config=excluded.match_config, webhook_config=excluded.webhook_config, position=excluded.position, updated_at=excluded.updated_at
   WHERE enabled != excluded.enabled OR match_config != excluded.match_config OR webhook_config != excluded.webhook_config OR position != excluded.position`, uuid.NewString(), r.Name, boolToInt(r.Enabled), string(match), string(wh), r.Position, now, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
