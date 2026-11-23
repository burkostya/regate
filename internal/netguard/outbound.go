// Package netguard implements a fail-closed outbound HTTP boundary for MCP and
// hook connections. It deliberately does not honor HTTP_PROXY: a proxy can
// otherwise bypass the DNS/IP checks performed here.
package netguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"
)

// External HTTPS hosts must be named explicitly in REGATE_ALLOWED_HOSTS.
// Loopback URLs are permitted for local development. Allowlisting a hostname
// never authorizes a connection to a private or metadata-service IP address.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("outbound destination must be an absolute HTTP(S) URL without userinfo")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return errors.New("outbound destination has no hostname")
	}
	if isLoopbackHost(host) {
		return nil
	}
	if u.Scheme != "https" {
		return fmt.Errorf("external HTTP forbidden: %s", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		return errors.New("non-loopback IP literals are forbidden")
	}
	for _, item := range strings.Split(os.Getenv("REGATE_ALLOWED_HOSTS"), ",") {
		if strings.EqualFold(strings.TrimSpace(strings.TrimSuffix(item, ".")), host) {
			return nil
		}
	}
	return fmt.Errorf("outbound host %s is not in REGATE_ALLOWED_HOSTS", host)
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

var blocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("ff00::/8"),
}

func permittedAddress(host string, ip net.IP) bool {
	if isLoopbackHost(host) {
		return ip.IsLoopback()
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range blocked {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

type lookupFunc func(context.Context, string) ([]net.IPAddr, error)
type dialFunc func(context.Context, string, string) (net.Conn, error)

func guardedDial(ctx context.Context, network, address string, lookup lookupFunc, dial dialFunc) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if host == "" || port == "" {
		return nil, errors.New("invalid outbound address")
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	// DialContext is a second security boundary: it protects against DNS
	// rebinding even when the request URL passed ValidateURL earlier.
	ips, err := lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("resolve %s: no addresses", host)
	}
	for _, addr := range ips {
		if !permittedAddress(host, addr.IP) {
			return nil, fmt.Errorf("outbound DNS result blocked: %s", host)
		}
	}
	var last error
	for _, addr := range ips {
		c, e := dial(ctx, network, net.JoinHostPort(addr.IP.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	return nil, fmt.Errorf("dial %s: %w", host, last)
}

type outboundTransport struct{ inner http.RoundTripper }

func (t outboundTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := ValidateURL(req.URL.String()); err != nil {
		return nil, err
	}
	return t.inner.RoundTrip(req)
}

// Client constructs an outbound client that pins validated DNS results at
// connection time and rejects redirects. Hostname and TLS SNI remain unchanged.
func Client(timeout time.Duration) *http.Client {
	return newClient(timeout, 0)
}

// StreamingClient bounds the wait for headers while leaving stream lifetime
// to the caller's request context.
func StreamingClient(headerTimeout time.Duration) *http.Client {
	return newClient(0, headerTimeout)
}

func newClient(timeout, headerTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	if headerTimeout > 0 {
		tr.ResponseHeaderTimeout = headerTimeout
		tr.DisableCompression = true
	}
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return guardedDial(ctx, network, address, net.DefaultResolver.LookupIPAddr, (&net.Dialer{}).DialContext)
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     outboundTransport{inner: tr},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}
