package analytics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"time"
)

// The archive endpoint comes from deploy.yml, which a project token sets, and clowdrd sends
// requests there from inside the server. So the endpoint must not reach what only the server can
// reach: loopback, private networks, link-local (the cloud metadata service at 169.254.169.254)
// and the like. The check runs on the address actually dialled, after DNS, so a name that
// resolves (or re-resolves) to such an address is refused too. The platform's admin allows an
// S3 on the same host by listing its host:port in platform.yml (analytics.archive.allow_private).

// blockedPrefixes are ranges no archive endpoint may dial, besides those net/netip names.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT, also some cloud internals
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("64:ff9b::/96"),  // NAT64: an IPv4 address in disguise
}

// privateAddr reports whether an address is one an archive endpoint may not dial.
func privateAddr(a netip.Addr) bool {
	a = a.Unmap()
	if a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() || !a.IsValid() {
		return true
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ErrPrivateEndpoint is an archive endpoint that resolves to an address the server keeps to itself.
var ErrPrivateEndpoint = fmt.Errorf("the archive endpoint resolves to a private, loopback or link-local address (allow it in platform.yml analytics.archive.allow_private)")

// guardedClient is an HTTP client that dials only public addresses, except the host:port pairs
// in allow. It ignores proxy settings, which would dial on its behalf.
func guardedClient(allow func() []string, timeout time.Duration) *http.Client {
	base := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	checked := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second,
		// Control sees the address being connected, after resolution: no rebinding gets past.
		Control: func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil || privateAddr(ap.Addr()) {
				return ErrPrivateEndpoint
			}
			return nil
		}}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if allow != nil {
				for _, a := range allow() {
					if strings.EqualFold(strings.TrimSpace(a), addr) {
						return base.DialContext(ctx, network, addr)
					}
				}
			}
			return checked.DialContext(ctx, network, addr)
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}
