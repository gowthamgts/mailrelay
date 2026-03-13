package rules

import (
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
)

// Engine evaluates emails against rules stored in the database.
type Engine struct {
	mu    sync.RWMutex
	rules []models.Rule
}

// NewEngine creates an empty rule engine. Call SetRules to populate it.
func NewEngine() *Engine {
	return &Engine{}
}

// SetRules replaces the current set of rules.
func (e *Engine) SetRules(rules []models.Rule) {
	e.mu.Lock()
	e.rules = rules
	e.mu.Unlock()
}

// Match returns all rules that match the given email.
// All non-empty matchers within a rule must match (AND logic).
// For multi-value fields (To addresses), any single match suffices (OR logic).
func (e *Engine) Match(email *models.ParsedEmail) []models.Rule {
	metrics.RulesEvaluatedTotal.Inc()

	e.mu.RLock()
	snapshot := e.rules
	e.mu.RUnlock()

	var matched []models.Rule
	for _, rule := range snapshot {
		if matchRule(rule.Match, email) {
			matched = append(matched, rule)
			metrics.RulesMatchedTotal.WithLabelValues(rule.Name).Inc()
		}
	}

	if len(matched) == 0 {
		metrics.RulesNoMatchTotal.Inc()
	}

	return matched
}

func matchRule(m models.MatcherConfig, email *models.ParsedEmail) bool {
	// Each non-empty field must match (AND logic).
	if m.FromEmail != "" {
		if !matchPattern(m.FromEmail, email.EnvelopeFrom) {
			return false
		}
	}

	if m.ToEmail != "" {
		if !matchAny(m.ToEmail, email.EnvelopeTo) {
			return false
		}
	}

	if m.Subject != "" {
		if !matchPattern(m.Subject, email.Subject) {
			return false
		}
	}

	if m.FromDomain != "" {
		if !matchPattern(m.FromDomain, email.FromDomain()) {
			return false
		}
	}

	if m.ToDomain != "" {
		if !matchAny(m.ToDomain, email.ToDomains()) {
			return false
		}
	}

	return true
}

// matchAny returns true if the pattern matches any value in the list (OR logic).
func matchAny(pattern string, values []string) bool {
	for _, v := range values {
		if matchPattern(pattern, v) {
			return true
		}
	}
	return false
}

// matchPattern checks a single value against a pattern.
// Patterns wrapped in /…/ are treated as regex; otherwise glob via path.Match.
func matchPattern(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	value = strings.TrimSpace(value)

	// Regex mode: /pattern/
	if len(pattern) >= 2 && pattern[0] == '/' && pattern[len(pattern)-1] == '/' {
		re, err := regexp.Compile(pattern[1 : len(pattern)-1])
		if err != nil {
			return false
		}
		return re.MatchString(value)
	}

	// Glob mode (case-insensitive).
	matched, err := path.Match(strings.ToLower(pattern), strings.ToLower(value))
	if err != nil {
		return false
	}
	return matched
}
