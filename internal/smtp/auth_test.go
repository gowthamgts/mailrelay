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

func TestCheckARCChain_WithPassingChain(t *testing.T) {
	raw := []byte(
		"From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Test\r\n" +
			"ARC-Seal: i=1; a=rsa-sha256; cv=pass; d=example.com; s=arc; b=abc\r\n" +
			"ARC-Authentication-Results: i=1; spf=pass smtp.mailfrom=user@example.com; dkim=pass header.d=example.com\r\n" +
			"\r\n" +
			"Body",
	)
	result := checkARCChain(raw)
	if !result.passed {
		t.Error("expected passed=true for valid ARC chain")
	}
	if result.spf != models.AuthPass {
		t.Errorf("spf = %q, want pass", result.spf)
	}
	if result.dkim != models.AuthPass {
		t.Errorf("dkim = %q, want pass", result.dkim)
	}
}

func TestCheckARCChain_WithFailingChain(t *testing.T) {
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

func TestCheckARCChain_MultipleSeals_UsesHighest(t *testing.T) {
	// Two ARC seals: i=1 cv=pass, i=2 cv=pass.
	// The highest is i=2, so the chain should pass.
	// Original SPF/DKIM is in ARC-Authentication-Results: i=1.
	raw := []byte(
		"From: sender@example.com\r\n" +
			"To: recipient@example.com\r\n" +
			"Subject: Test\r\n" +
			"ARC-Seal: i=1; cv=pass; a=rsa-sha256; d=hop1.example.com; s=arc; b=sig1\r\n" +
			"ARC-Seal: i=2; cv=pass; a=rsa-sha256; d=hop2.example.com; s=arc; b=sig2\r\n" +
			"ARC-Authentication-Results: i=1; spf=pass smtp.mailfrom=user@example.com; dkim=fail\r\n" +
			"ARC-Authentication-Results: i=2; spf=fail smtp.mailfrom=user@example.com; dkim=pass\r\n" +
			"\r\n" +
			"Body",
	)
	result := checkARCChain(raw)
	if !result.passed {
		t.Error("expected passed=true with two passing ARC seals")
	}
	// Should use i=1 original results.
	if result.spf != models.AuthPass {
		t.Errorf("spf = %q, want pass (from i=1)", result.spf)
	}
	if result.dkim != models.AuthFail {
		t.Errorf("dkim = %q, want fail (from i=1)", result.dkim)
	}
}

func TestCheckARCChain_InvalidMessage(t *testing.T) {
	result := checkARCChain([]byte("not a valid email"))
	// Should not panic; may or may not pass depending on parsing.
	// The important thing is it doesn't crash.
	_ = result
}
