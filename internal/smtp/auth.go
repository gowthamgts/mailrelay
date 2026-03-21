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
	dmarc  models.AuthCheckResult
}

// checkARCChain parses ARC-Seal and ARC-Authentication-Results headers to
// determine whether the ARC chain is intact. If the highest-numbered
// ARC-Seal has a valid cv value, the chain is valid and the original
// SPF/DKIM/DMARC results are extracted from ARC-Authentication-Results: i=1.
//
// Per RFC 8617:
//   - i=1 MUST have cv=none (no prior ARC chain existed).
//   - i>1 MUST have cv=pass (prior chain was validated by the previous hop).
//   - Any cv=fail on any seal invalidates the entire chain (RFC 8617 §5.2).
func checkARCChain(rawEmail []byte) arcChainResult {
	msg, err := mail.ReadMessage(bytes.NewReader(rawEmail))
	if err != nil {
		return arcChainResult{}
	}

	// Per RFC 8617 §5.2: if any ARC-Seal has cv=fail, the whole chain is
	// invalid — a downstream MTA already detected a broken chain.
	for _, seal := range msg.Header["Arc-Seal"] {
		if strings.EqualFold(arcStringParam(seal, "cv"), "fail") {
			return arcChainResult{}
		}
	}

	// Find the highest ARC instance with a valid cv value.
	// Per RFC 8617: i=1 MUST have cv=none; i>1 MUST have cv=pass.
	maxValidInstance := 0
	for _, seal := range msg.Header["Arc-Seal"] {
		i := arcIntParam(seal, "i")
		cv := arcStringParam(seal, "cv")
		validCV := strings.EqualFold(cv, "pass") || (i == 1 && strings.EqualFold(cv, "none"))
		if i > 0 && validCV && i > maxValidInstance {
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
			res.dmarc = authResultValue(aar, "dmarc")
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

// parseHeaderFromDomain extracts the domain from the email's RFC5322 From:
// header. DMARC requires the lookup and alignment check to use this domain,
// not the SMTP envelope from domain (RFC 7489 §6.6.1).
func parseHeaderFromDomain(rawEmail []byte) string {
	msg, err := mail.ReadMessage(bytes.NewReader(rawEmail))
	if err != nil {
		return ""
	}
	fromHdr := msg.Header.Get("From")
	if fromHdr == "" {
		return ""
	}
	addrs, err := mail.ParseAddressList(fromHdr)
	if err != nil || len(addrs) == 0 {
		return ""
	}
	parts := strings.SplitN(addrs[0].Address, "@", 2)
	if len(parts) == 2 {
		return strings.ToLower(parts[1])
	}
	return ""
}

// orgDomain returns the organisational domain (eTLD+1 approximation) used in
// DMARC relaxed-mode alignment. Without a Public Suffix List we approximate
// by taking the last two labels (e.g. mail.example.com → example.com). This
// is correct for most gTLDs (.com/.net/.org) but may be wrong for some
// two-level ccTLDs (e.g. .co.uk). We accept this limitation rather than
// pulling in a full PSL dependency.
func orgDomain(domain string) string {
	parts := strings.Split(strings.ToLower(domain), ".")
	if len(parts) <= 2 {
		return strings.ToLower(domain)
	}
	return strings.Join(parts[len(parts)-2:], ".")
}

// domainAligns reports whether checkDomain passes DMARC alignment against
// fromDomain. In relaxed mode (default) the organisational domains must match
// (RFC 7489 §3.1). In strict mode the domains must be identical.
func domainAligns(checkDomain, fromDomain string, relaxed bool) bool {
	a := strings.ToLower(checkDomain)
	b := strings.ToLower(fromDomain)
	if a == b {
		return true
	}
	if relaxed {
		return orgDomain(a) == orgDomain(b)
	}
	return false
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

	// envelopeFromDomain is from the SMTP MAIL FROM command, used for SPF
	// and SPF-alignment in DMARC.
	envelopeFromDomain := ""
	if parts := strings.SplitN(from, "@", 2); len(parts) == 2 {
		envelopeFromDomain = parts[1]
	}

	// hdrFromDomain is from the RFC5322 From: header, used for DMARC lookup
	// and alignment (RFC 7489 §6.6.1). Fall back to envelope domain if the
	// header is absent or unparseable.
	hdrFromDomain := parseHeaderFromDomain(rawEmail)
	if hdrFromDomain == "" {
		hdrFromDomain = envelopeFromDomain
	}

	// ARC is always checked — it is not user-configurable. It must run before
	// SPF/DKIM so that forwarded emails can use the ARC-preserved original auth
	// results instead of a fresh live check against the forwarder's IP.
	arc := checkARCChain(rawEmail)
	if arc.passed {
		result.ARC = models.AuthPass
		slog.Info("ARC chain passed, will use preserved auth results", "from", from)
	}
	metrics.AuthChecksTotal.WithLabelValues("arc", string(result.ARC)).Inc()

	// SPF check (RFC 7208).
	// For forwarded emails with a valid ARC chain, use the ARC-preserved
	// original SPF result rather than checking the forwarder's IP.
	if cfg.SPF.Enabled() && ip != nil && envelopeFromDomain != "" {
		if arc.passed && arc.spf != models.AuthNone {
			result.SPF = arc.spf
			slog.Info("SPF result (ARC-preserved)", "result", result.SPF, "from", from)
		} else {
			spfResult, err := spf.CheckHostWithSender(ip, envelopeFromDomain, from)
			if err != nil {
				slog.Warn("SPF check error", "error", err, "from", from)
			}
			switch spfResult {
			case spf.Pass:
				result.SPF = models.AuthPass
			case spf.Neutral, spf.None:
				// Neutral/None: domain makes no assertion; not a failure.
				result.SPF = models.AuthNone
			case spf.TempError:
				// RFC 7208 §2.6.6: transient DNS error; MUST NOT be treated as fail.
				result.SPF = models.AuthNone
				slog.Warn("SPF TempError (transient DNS failure)", "from", from)
			case spf.PermError:
				// RFC 7208 §2.6.7: permanent record error; treat as none, not fail.
				result.SPF = models.AuthNone
				slog.Warn("SPF PermError (misconfigured SPF record)", "from", from)
			default: // spf.Fail, spf.SoftFail
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

	// DKIM check (RFC 6376).
	// Per RFC 6376 §6.1: a message is considered authenticated by DKIM if at
	// least one signature verifies successfully. We also collect the signing
	// domains of all passing signatures for DMARC alignment below.
	var dkimPassDomains []string
	if cfg.DKIM.Enabled() {
		verifications, err := dkim.Verify(bytes.NewReader(rawEmail))
		if err != nil {
			slog.Warn("DKIM verification error", "error", err)
		}
		if len(verifications) == 0 {
			result.DKIM = models.AuthNone
		} else {
			for _, v := range verifications {
				if v.Err == nil {
					result.DKIM = models.AuthPass
					dkimPassDomains = append(dkimPassDomains, strings.ToLower(v.Domain))
				} else {
					slog.Warn("DKIM signature failed", "domain", v.Domain, "error", v.Err)
				}
			}
			if result.DKIM != models.AuthPass {
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

	// DMARC check (RFC 7489).
	// The lookup domain is the RFC5322 header From domain, not the envelope
	// from domain (RFC 7489 §6.6.1).
	// A message passes DMARC if SPF or DKIM passes with proper identifier
	// alignment to the header From domain (RFC 7489 §3.1).
	if cfg.DMARC.Enabled() && hdrFromDomain != "" {
		// For forwarded emails with a valid ARC chain, use the DMARC result
		// preserved by the original receiving MTA if available. That server
		// already performed the correct alignment check against the original
		// message.
		if arc.passed && arc.dmarc != models.AuthNone {
			result.DMARC = arc.dmarc
			slog.Info("DMARC result (ARC-preserved)", "result", result.DMARC, "from", from)
		} else {
			record, err := dmarc.Lookup(hdrFromDomain)
			if err != nil {
				if dmarc.IsTempFail(err) {
					slog.Warn("DMARC lookup temp failure", "domain", hdrFromDomain, "error", err)
				}
				result.DMARC = models.AuthNone
			} else {
				spfRelaxed := record.SPFAlignment != dmarc.AlignmentStrict
				dkimRelaxed := record.DKIMAlignment != dmarc.AlignmentStrict

				// SPF alignment: envelope-from domain must align with header-from
				// domain (RFC 7489 §3.1.1).
				spfAligned := result.SPF == models.AuthPass &&
					domainAligns(envelopeFromDomain, hdrFromDomain, spfRelaxed)

				// DKIM alignment: at least one passing DKIM signature's d= domain
				// must align with the header-from domain (RFC 7489 §3.1.2).
				dkimAligned := false
				for _, d := range dkimPassDomains {
					if domainAligns(d, hdrFromDomain, dkimRelaxed) {
						dkimAligned = true
						break
					}
				}

				if spfAligned || dkimAligned {
					result.DMARC = models.AuthPass
				} else {
					result.DMARC = models.AuthFail
				}
			}
			slog.Info("DMARC result", "result", result.DMARC, "from", from)
		}
		metrics.AuthChecksTotal.WithLabelValues("dmarc", string(result.DMARC)).Inc()
		if cfg.DMARC.Enforced() && result.DMARC == models.AuthFail {
			metrics.AuthEnforcementFailuresTotal.WithLabelValues("dmarc").Inc()
			return result, fmt.Errorf("DMARC check failed for %s", from)
		}
	}

	return result, nil
}
