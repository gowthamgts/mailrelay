package webhook

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"syscall"
	"time"
)

// ErrPrivateAddress is returned when a webhook resolves to an internal address
// and private networks are not allowed.
var ErrPrivateAddress = errors.New("webhook address is private or internal; set webhook.allow_private_networks to true to allow it")

// Ranges not covered by the net.IP helpers in isInternalIP.
var extraInternalNets = mustParseCIDRs(
	"0.0.0.0/8",     // "this network"
	"100.64.0.0/10", // carrier-grade NAT, also used by Tailscale
)

// isInternalIP reports whether ip is loopback, private (RFC 1918 / RFC 4193),
// link-local (including cloud metadata at 169.254.169.254), multicast,
// unspecified, or in one of extraInternalNets.
func isInternalIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	for _, n := range extraInternalNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// newTransport returns an HTTP transport that refuses to connect to internal
// addresses unless allowPrivate is set. The check runs on the resolved IP at
// dial time, so it also covers DNS names pointing inward and redirects.
func newTransport(allowPrivate *atomic.Bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			if allowPrivate.Load() {
				return nil
			}
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || isInternalIP(ip) {
				return fmt.Errorf("%w (%s)", ErrPrivateAddress, host)
			}
			return nil
		},
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = dialer.DialContext
	return t
}

func mustParseCIDRs(cidrs ...string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		nets = append(nets, n)
	}
	return nets
}
