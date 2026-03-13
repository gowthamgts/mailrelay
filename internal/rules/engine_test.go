package rules

import (
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		value   string
		want    bool
	}{
		{"glob exact", "user@example.com", "user@example.com", true},
		{"glob case insensitive", "USER@EXAMPLE.COM", "user@example.com", true},
		{"glob wildcard", "*@example.com", "anyone@example.com", true},
		{"glob no match", "user@example.com", "other@example.com", false},
		{"regex match", "/^user@/", "user@example.com", true},
		{"regex no match", "/^admin@/", "user@example.com", false},
		{"regex invalid returns false", "/[invalid/", "anything", false},
		{"wildcard star", "*", "anything", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchPattern(tt.pattern, tt.value)
			if got != tt.want {
				t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.value, got, tt.want)
			}
		})
	}
}

func TestMatchAny(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		values  []string
		want    bool
	}{
		{"matches one of many", "*@example.com", []string{"a@foo.com", "b@example.com"}, true},
		{"matches none", "*@example.com", []string{"a@foo.com", "b@bar.com"}, false},
		{"empty values", "*@example.com", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchAny(tt.pattern, tt.values)
			if got != tt.want {
				t.Errorf("matchAny(%q, %v) = %v, want %v", tt.pattern, tt.values, got, tt.want)
			}
		})
	}
}

func TestMatchRule(t *testing.T) {
	email := &models.ParsedEmail{
		EnvelopeFrom: "sender@example.com",
		EnvelopeTo:   []string{"recipient@dest.com"},
		Subject:      "Hello World",
	}

	tests := []struct {
		name  string
		match models.MatcherConfig
		want  bool
	}{
		{
			"empty matchers match everything",
			models.MatcherConfig{},
			true,
		},
		{
			"AND logic all match",
			models.MatcherConfig{
				FromEmail: "*@example.com",
				ToEmail:   "*@dest.com",
				Subject:   "Hello*",
			},
			true,
		},
		{
			"AND logic one fails",
			models.MatcherConfig{
				FromEmail: "*@example.com",
				ToEmail:   "*@other.com",
			},
			false,
		},
		{
			"from_domain match",
			models.MatcherConfig{FromDomain: "example.com"},
			true,
		},
		{
			"to_domain match",
			models.MatcherConfig{ToDomain: "dest.com"},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchRule(tt.match, email)
			if got != tt.want {
				t.Errorf("matchRule() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEngineMatch(t *testing.T) {
	testRules := []models.Rule{
		{Name: "rule1", Match: models.MatcherConfig{FromDomain: "example.com"}},
		{Name: "rule2", Match: models.MatcherConfig{FromDomain: "other.com"}},
		{Name: "rule3", Match: models.MatcherConfig{Subject: "*World*"}},
	}
	engine := NewEngine()
	engine.SetRules(testRules)

	email := &models.ParsedEmail{
		EnvelopeFrom: "sender@example.com",
		EnvelopeTo:   []string{"r@dest.com"},
		Subject:      "Hello World",
	}

	matched := engine.Match(email)
	if len(matched) != 2 {
		t.Fatalf("expected 2 matches, got %d: %v", len(matched), matched)
	}
	if matched[0].Name != "rule1" {
		t.Errorf("first match = %q, want rule1", matched[0].Name)
	}
	if matched[1].Name != "rule3" {
		t.Errorf("second match = %q, want rule3", matched[1].Name)
	}

	// No matches.
	noMatchEmail := &models.ParsedEmail{
		EnvelopeFrom: "sender@nowhere.com",
		EnvelopeTo:   []string{"r@dest.com"},
		Subject:      "Nothing",
	}
	if got := engine.Match(noMatchEmail); got != nil {
		t.Errorf("expected nil matches, got %v", got)
	}
}
