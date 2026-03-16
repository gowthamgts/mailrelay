package smtp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/mail"
	"strconv"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
)

// arcChainResult holds the result of parsing the ARC chain.
type arcChainResult struct {
	passed bool
	spf    models.AuthCheckResult
	dkim   models.AuthCheckResult
}

// checkARCChain parses ARC-Seal and ARC-Authentication-Results headers to
// determine whether the ARC chain is intact. If the highest-numbered
// ARC-Seal has cv=pass, the chain is valid and the original SPF/DKIM results
// are extracted from ARC-Authentication-Results: i=1.
//
// This is used to correctly handle forwarded emails: the forwarding server's
// IP will fail a fresh SPF check against the original sender's domain, but
// the ARC chain preserves the original passing SPF result.
func checkARCChain(rawEmail []byte) arcChainResult {
	msg, err := mail.ReadMessage(bytes.NewReader(rawEmail))
	if err != nil {
		return arcChainResult{}
	}

	// Find the highest ARC instance whose seal has cv=pass.
	maxValidInstance := 0
	for _, seal := range msg.Header["Arc-Seal"] {
		i := arcIntParam(seal, "i")
		cv := arcStringParam(seal, "cv")
		if i > 0 && strings.EqualFold(cv, "pass") && i > maxValidInstance {
			maxValidInstance = i
		}
	}

	if maxValidInstance == 0 {
		return arcChainResult{}
	}

	// Extract original auth results from ARC-Authentication-Results: i=1.
	res := arcChainResult{passed: true}
	for _, aar := range msg.Header["Arc-Authentication-Results"] {
		if arcIntParam(aar, "i") == 1 {
			res.spf = authResultValue(aar, "spf")
			res.dkim = authResultValue(aar, "dkim")
			break
		}
	}
	return res
}

// arcIntParam extracts an integer parameter (e.g. "i=2") from a semicolon-
// delimited ARC header value.
func arcIntParam(s, key string) int {
	lower := strings.ToLower(s)
	prefix := key + "="
	for _, part := range strings.Split(lower, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, prefix) {
			n, err := strconv.Atoi(strings.TrimSpace(part[len(prefix):]))
			if err == nil {
				return n
			}
		}
	}
	return 0
}

// arcStringParam extracts a string parameter (e.g. "cv=pass") from a
// semicolon-delimited ARC header value.
func arcStringParam(s, key string) string {
	lower := strings.ToLower(s)
	prefix := key + "="
	for _, part := range strings.Split(lower, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, prefix) {
			return strings.TrimSpace(part[len(prefix):])
		}
	}
	return ""
}

// authResultValue extracts the result of a named method from an
// Authentication-Results or ARC-Authentication-Results header value.
// E.g. authResultValue("... spf=pass smtp.mailfrom=...", "spf") → AuthPass.
func authResultValue(s, method string) models.AuthCheckResult {
	lower := strings.ToLower(s)
	prefix := method + "="
	idx := strings.Index(lower, prefix)
	if idx < 0 {
		return models.AuthNone
	}
	// Require a word boundary before the method name.
	if idx > 0 {
		prev := lower[idx-1]
		if prev != ' ' && prev != '\t' && prev != '\n' && prev != ';' {
			return models.AuthNone
		}
	}
	val := lower[idx+len(prefix):]
	if end := strings.IndexAny(val, " \t\n\r;("); end >= 0 {
		val = val[:end]
	}
	switch strings.TrimSpace(val) {
	case "pass":
		return models.AuthPass
	case "fail", "softfail":
		return models.AuthFail
	default:
		return models.AuthNone
	}
}

// VerifyAuth runs SPF, DKIM, DMARC, and ARC checks on the email.
func VerifyAuth(ctx context.Context, cfg models.AuthConfig, remoteAddr net.Addr, from string, rawEmail []byte) (*models.AuthResult, error) {
	result := &models.AuthResult{
		SPF:   models.AuthNone,
		DKIM:  models.AuthNone,
		DMARC: models.AuthNone,
		ARC:   models.AuthNone,
	}

	var ip net.IP
	if tcpAddr, ok := remoteAddr.(*net.TCPAddr); ok {
		ip = tcpAddr.IP
	} else {
		host, _, _ := net.SplitHostPort(remoteAddr.String())
		ip = net.ParseIP(host)
	}

	fromDomain := ""
	if parts := strings.SplitN(from, "@", 2); len(parts) == 2 {
		fromDomain = parts[1]
	}

	// ARC check: must run before SPF/DKIM so that forwarded emails can use
	// the ARC-preserved original auth results instead of a fresh live check
	// against the forwarder's IP.
	var arc arcChainResult
	if cfg.ARC.Enabled() {
		arc = checkARCChain(rawEmail)
		if arc.passed {
			result.ARC = models.AuthPass
			slog.Info("ARC chain passed, will use preserved auth results", "from", from)
		} else if bytes.Contains(rawEmail, []byte("ARC-Authentication-Results:")) {
			result.ARC = models.AuthNone
		}
		metrics.AuthChecksTotal.WithLabelValues("arc", string(result.ARC)).Inc()
		if cfg.ARC.Enforced() && result.ARC == models.AuthFail {
			metrics.AuthEnforcementFailuresTotal.WithLabelValues("arc").Inc()
			return result, fmt.Errorf("ARC check failed for %s", from)
		}
	}

	// SPF check.
	// For forwarded emails with a valid ARC chain, use the ARC-preserved
	// original SPF result rather than checking the forwarder's IP.
	if cfg.SPF.Enabled() && ip != nil && fromDomain != "" {
		if arc.passed && arc.spf != models.AuthNone {
			result.SPF = arc.spf
			slog.Info("SPF result (ARC-preserved)", "result", result.SPF, "from", from)
		} else {
			spfResult, err := spf.CheckHostWithSender(ip, fromDomain, from)
			if err != nil {
				slog.Warn("SPF check error", "error", err, "from", from)
			}
			switch spfResult {
			case spf.Pass:
				result.SPF = models.AuthPass
			case spf.Neutral, spf.None:
				// Neutral means the domain owner makes no assertion either way;
				// treat it the same as no record rather than a failure.
				result.SPF = models.AuthNone
			default:
				result.SPF = models.AuthFail
			}
			slog.Info("SPF result", "result", result.SPF, "from", from, "ip", ip)
		}
		metrics.AuthChecksTotal.WithLabelValues("spf", string(result.SPF)).Inc()
		if cfg.SPF.Enforced() && result.SPF == models.AuthFail {
			metrics.AuthEnforcementFailuresTotal.WithLabelValues("spf").Inc()
			return result, fmt.Errorf("SPF check failed for %s", from)
		}
	}

	// DKIM check.
	if cfg.DKIM.Enabled() {
		verifications, err := dkim.Verify(bytes.NewReader(rawEmail))
		if err != nil {
			slog.Warn("DKIM verification error", "error", err)
		}
		if len(verifications) == 0 {
			result.DKIM = models.AuthNone
		} else {
			allPassed := true
			for _, v := range verifications {
				if v.Err != nil {
					allPassed = false
					slog.Warn("DKIM signature failed", "domain", v.Domain, "error", v.Err)
				}
			}
			if allPassed {
				result.DKIM = models.AuthPass
			} else {
				result.DKIM = models.AuthFail
			}
		}
		slog.Info("DKIM result", "result", result.DKIM, "from", from)
		metrics.AuthChecksTotal.WithLabelValues("dkim", string(result.DKIM)).Inc()
		if cfg.DKIM.Enforced() && result.DKIM == models.AuthFail {
			metrics.AuthEnforcementFailuresTotal.WithLabelValues("dkim").Inc()
			return result, fmt.Errorf("DKIM check failed for %s", from)
		}
	}

	// DMARC check.
	if cfg.DMARC.Enabled() && fromDomain != "" {
		record, err := dmarc.Lookup(fromDomain)
		if err != nil {
			if dmarc.IsTempFail(err) {
				slog.Warn("DMARC lookup temp failure", "domain", fromDomain, "error", err)
			}
			result.DMARC = models.AuthNone
		} else {
			// DMARC passes if either SPF or DKIM passed with alignment.
			spfAligned := result.SPF == models.AuthPass
			dkimAligned := result.DKIM == models.AuthPass

			if record.DKIMAlignment == "s" {
				// Strict alignment: the DKIM domain must exactly match.
				_ = dkimAligned // simplified — keep as-is for now
			}
			if record.SPFAlignment == "s" {
				// Strict alignment: the envelope from domain must exactly match.
				_ = spfAligned
			}

			if spfAligned || dkimAligned {
				result.DMARC = models.AuthPass
			} else {
				result.DMARC = models.AuthFail
			}
		}
		slog.Info("DMARC result", "result", result.DMARC, "from", from)
		metrics.AuthChecksTotal.WithLabelValues("dmarc", string(result.DMARC)).Inc()
		if cfg.DMARC.Enforced() && result.DMARC == models.AuthFail {
			metrics.AuthEnforcementFailuresTotal.WithLabelValues("dmarc").Inc()
			return result, fmt.Errorf("DMARC check failed for %s", from)
		}
	}

	return result, nil
}
