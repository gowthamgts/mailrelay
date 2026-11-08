package models

// TransferRule contains portable rule settings, without database identity or timestamps.
type TransferRule struct {
	Name     string        `json:"name"`
	Enabled  bool          `json:"enabled"`
	Position int           `json:"position"`
	Match    MatcherConfig `json:"match"`
	Webhook  WebhookConfig `json:"webhook"`
}
