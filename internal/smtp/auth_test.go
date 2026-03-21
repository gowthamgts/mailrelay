package smtp

import (
	"testing"

	"github.com/gowthamgts/mailrelay/internal/models"
)

func TestArcIntParam(t *testing.T) {
	tests := []struct {
		input string
		key   string
		want  int
	}{
		{"i=1; a=rsa-sha256; cv=none", "i", 1},
		{"a=rsa-sha256; i=3; cv=pass", "i", 3},
		{"cv=pass; i=2", "i", 2},
		{"cv=pass", "i", 0},          // missing key
		{"i=abc; cv=pass", "i", 0},   // non-integer value
		{"i=; cv=pass", "i", 0},      // empty value
		{"xi=5; cv=pass", "i", 0},    // prefix boundary: "xi=" should not match "i="
		{"I=5; cv=pass", "i", 5},     // case-insensitive
	}

	for _, tt := range tests {
		got := arcIntParam(tt.input, tt.key)
		if got != tt.want {
			t.Errorf("arcIntParam(%q, %q) = %d, want %d", tt.input, tt.key, got, tt.want)
		}
	}
}

func TestArcStringParam(t *testing.T) {
	tests := []struct {
		input string
		key   string
		want  string
	}{
		{"i=1; a=rsa-sha256; cv=none", "cv", "none"},
		{"cv=pass; i=2", "cv", "pass"},
		{"i=1; cv=fail", "cv", "fail"},
		{"i=1; a=rsa-sha256", "cv", ""},  // missing key
		{"CV=PASS; i=1", "cv", "pass"},   // case-insensitive
		{"cv=; i=1", "cv", ""},           // empty value
	}

	for _, tt := range tests {
		got := arcStringParam(tt.input, tt.key)
		if got != tt.want {
			t.Errorf("arcStringParam(%q, %q) = %q, want %q", tt.input, tt.key, got, tt.want)
		}
	}
}

func TestAuthResultValue(t *testing.T) {
	tests := []struct {
		input  string
		method string
		want   models.AuthCheckResult
	}{
		{"spf=pass smtp.mailfrom=user@example.com", "spf", models.AuthPass},
		{"spf=fail smtp.mailfrom=user@example.com", "spf", models.AuthFail},
		{"spf=softfail smtp.mailfrom=user@example.com", "spf", models.AuthFail},
		{"dkim=pass header.d=example.com", "dkim", models.AuthPass},
		{"dkim=fail header.d=example.com", "dkim", models.AuthFail},
		{"dmarc=pass", "dmarc", models.AuthPass},
		{"dmarc=fail", "dmarc", models.AuthFail},
		{"spf=none smtp.mailfrom=x", "spf", models.AuthNone},
		// Method not present → none.
		{"dkim=pass header.d=example.com", "spf", models.AuthNone},
		// Word boundary: "myspf=pass" should NOT match "spf".
		{"myspf=pass", "spf", models.AuthNone},
		// With semicolon separator.
		{"; spf=pass smtp.mailfrom=x", "spf", models.AuthPass},
		// Whitespace before method.
		{"  spf=pass", "spf", models.AuthPass},
	}

	for _, tt := range tests {
		got := authResultValue(tt.input, tt.method)
		if got != tt.want {
			t.Errorf("authResultValue(%q, %q) = %q, want %q", tt.input, tt.method, got, tt.want)
		}
	}
}

func TestCheckARCChain_NoHeaders(t *testing.T) {
	raw := []byte("From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: Test\r\n\r\nBody")
	result := checkARCChain(raw)
	if result.passed {
		t.Error("expected passed=false for email without ARC headers")
	}
}

func TestCheckARCChain_SingleHopCvNone(t *testing.T) {
	// RFC 8617: i=1 MUST have cv=none (no prior chain existed). This is the
	// real-world case for a single forwarding hop (e.g. Fastmail).
	raw := []byte(
		"From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Forwarded\r\n" +
			"ARC-Seal: i=1; a=rsa-sha256; cv=none; d=forwarder.example.com; s=arc; b=sig\r\n" +
			"ARC-Authentication-Results: i=1; forwarder.example.com; spf=pass smtp.mailfrom=user@example.com; dkim=pass header.d=example.com; dmarc=pass\r\n" +
			"\r\n" +
			"Body",
	)
	result := checkARCChain(raw)
	if !result.passed {
		t.Error("expected passed=true: cv=none at i=1 is a valid ARC chain start")
	}
	if result.spf != models.AuthPass {
		t.Errorf("spf = %q, want pass", result.spf)
	}
	if result.dkim != models.AuthPass {
		t.Errorf("dkim = %q, want pass", result.dkim)
	}
	if result.dmarc != models.AuthPass {
		t.Errorf("dmarc = %q, want pass", result.dmarc)
	}
}

func TestCheckARCChain_WithFailingChain(t *testing.T) {
	// cv=fail at any instance must invalidate the entire chain (RFC 8617 §5.2).
	raw := []byte(
		"From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Test\r\n" +
			"ARC-Seal: i=1; a=rsa-sha256; cv=fail; d=example.com; s=arc; b=abc\r\n" +
			"ARC-Authentication-Results: i=1; spf=pass\r\n" +
			"\r\n" +
			"Body",
	)
	result := checkARCChain(raw)
	if result.passed {
		t.Error("expected passed=false when cv=fail")
	}
}

func TestCheckARCChain_CvFailOnHigherInstanceInvalidatesChain(t *testing.T) {
	// Even if i=1 is valid (cv=none), a cv=fail at i=2 means a downstream
	// MTA found the chain broken — the entire chain must be rejected
	// (RFC 8617 §5.2).
	raw := []byte(
		"From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Test\r\n" +
			"ARC-Seal: i=1; a=rsa-sha256; cv=none; d=hop1.example.com; s=arc; b=sig1\r\n" +
			"ARC-Seal: i=2; a=rsa-sha256; cv=fail; d=hop2.example.com; s=arc; b=sig2\r\n" +
			"ARC-Authentication-Results: i=1; spf=pass smtp.mailfrom=user@example.com\r\n" +
			"ARC-Authentication-Results: i=2; spf=fail\r\n" +
			"\r\n" +
			"Body",
	)
	result := checkARCChain(raw)
	if result.passed {
		t.Error("expected passed=false: cv=fail at i=2 must invalidate the whole chain")
	}
}

func TestCheckARCChain_MultipleSeals_UsesHighest(t *testing.T) {
	// Two-hop ARC chain: i=1 cv=none (RFC-correct), i=2 cv=pass.
	// The highest valid instance is i=2 so the chain passes.
	// Original SPF/DKIM/DMARC results come from ARC-Authentication-Results: i=1.
	raw := []byte(
		"From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Test\r\n" +
			"ARC-Seal: i=1; cv=none; a=rsa-sha256; d=hop1.example.com; s=arc; b=sig1\r\n" +
			"ARC-Seal: i=2; cv=pass; a=rsa-sha256; d=hop2.example.com; s=arc; b=sig2\r\n" +
			"ARC-Authentication-Results: i=1; spf=pass smtp.mailfrom=user@example.com; dkim=fail; dmarc=fail\r\n" +
			"ARC-Authentication-Results: i=2; spf=fail smtp.mailfrom=user@example.com; dkim=pass; dmarc=pass\r\n" +
			"\r\n" +
			"Body",
	)
	result := checkARCChain(raw)
	if !result.passed {
		t.Error("expected passed=true with two-hop ARC chain")
	}
	// Must use i=1 original results, not i=2.
	if result.spf != models.AuthPass {
		t.Errorf("spf = %q, want pass (from i=1)", result.spf)
	}
	if result.dkim != models.AuthFail {
		t.Errorf("dkim = %q, want fail (from i=1)", result.dkim)
	}
	if result.dmarc != models.AuthFail {
		t.Errorf("dmarc = %q, want fail (from i=1)", result.dmarc)
	}
}

func TestCheckARCChain_InvalidMessage(t *testing.T) {
	result := checkARCChain([]byte("not a valid email"))
	// Should not panic; may or may not pass depending on parsing.
	_ = result
}

func TestParseHeaderFromDomain(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		want string
	}{
		{
			name: "simple address",
			raw:  []byte("From: user@example.com\r\nSubject: Test\r\n\r\nBody"),
			want: "example.com",
		},
		{
			name: "display name",
			raw:  []byte("From: Alice <alice@alice.example.com>\r\nSubject: Test\r\n\r\nBody"),
			want: "alice.example.com",
		},
		{
			name: "no from header",
			raw:  []byte("Subject: Test\r\n\r\nBody"),
			want: "",
		},
		{
			name: "uppercase domain",
			raw:  []byte("From: user@EXAMPLE.COM\r\nSubject: Test\r\n\r\nBody"),
			want: "example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseHeaderFromDomain(tt.raw)
			if got != tt.want {
				t.Errorf("parseHeaderFromDomain() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOrgDomain(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"example.com", "example.com"},
		{"mail.example.com", "example.com"},
		{"sub.mail.example.com", "example.com"},
		{"com", "com"},
		{"", ""},
	}
	for _, tt := range tests {
		got := orgDomain(tt.input)
		if got != tt.want {
			t.Errorf("orgDomain(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestDomainAligns(t *testing.T) {
	tests := []struct {
		check   string
		from    string
		relaxed bool
		want    bool
	}{
		// Exact match always passes.
		{"example.com", "example.com", false, true},
		{"example.com", "example.com", true, true},
		// Relaxed: subdomain of from-domain passes.
		{"mail.example.com", "example.com", true, true},
		{"sub.mail.example.com", "example.com", true, true},
		// Strict: subdomain of from-domain fails.
		{"mail.example.com", "example.com", false, false},
		// Different domains always fail.
		{"other.com", "example.com", false, false},
		{"other.com", "example.com", true, false},
		// Case-insensitive.
		{"EXAMPLE.COM", "example.com", false, true},
		{"Mail.Example.COM", "example.com", true, true},
	}
	for _, tt := range tests {
		got := domainAligns(tt.check, tt.from, tt.relaxed)
		if got != tt.want {
			t.Errorf("domainAligns(%q, %q, relaxed=%v) = %v, want %v",
				tt.check, tt.from, tt.relaxed, got, tt.want)
		}
	}
}
