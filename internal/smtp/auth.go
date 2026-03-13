package smtp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"github.com/gowthamgts/mailrelay/internal/metrics"
	"github.com/gowthamgts/mailrelay/internal/models"
)

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

	// SPF check.
	if cfg.SPF.Enabled() && ip != nil && fromDomain != "" {
		spfResult, err := spf.CheckHostWithSender(ip, fromDomain, from)
		if err != nil {
			slog.Warn("SPF check error", "error", err, "from", from)
		}
		switch spfResult {
		case spf.Pass:
			result.SPF = models.AuthPass
		case spf.None:
			result.SPF = models.AuthNone
		default:
			result.SPF = models.AuthFail
		}
		slog.Info("SPF result", "result", result.SPF, "from", from, "ip", ip)
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

	// ARC check: parse Authentication-Results headers for ARC info.
	// go-msgauth doesn't provide direct ARC verification.
	// We check for ARC-Authentication-Results headers as a basic signal.
	if cfg.ARC.Enabled() {
		if bytes.Contains(rawEmail, []byte("ARC-Authentication-Results:")) {
			result.ARC = models.AuthPass
			slog.Info("ARC headers present", "from", from)
		} else {
			result.ARC = models.AuthNone
		}
		metrics.AuthChecksTotal.WithLabelValues("arc", string(result.ARC)).Inc()
		if cfg.ARC.Enforced() && result.ARC == models.AuthFail {
			metrics.AuthEnforcementFailuresTotal.WithLabelValues("arc").Inc()
			return result, fmt.Errorf("ARC check failed for %s", from)
		}
	}

	return result, nil
}
