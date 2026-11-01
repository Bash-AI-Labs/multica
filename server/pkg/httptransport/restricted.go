package httptransport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

var ErrAddressBlocked = errors.New("outbound HTTP address is not allowed")

var sharedAddressPrefix = netip.MustParsePrefix("100.64.0.0/10")

var blockedPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	sharedAddressPrefix,
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fec0::/10"),
}

func PublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !dialableAddress(address) || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
		address.IsInterfaceLocalMulticast() {
		return false
	}
	for _, prefix := range blockedPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return address.IsGlobalUnicast()
}

func dialableAddress(address netip.Addr) bool {
	return address.IsValid() && !address.IsUnspecified() && !address.IsMulticast()
}

func privateAddress(address netip.Addr) bool {
	address = address.Unmap()
	return dialableAddress(address) &&
		(address.IsPrivate() || address.IsLoopback() ||
			address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() ||
			sharedAddressPrefix.Contains(address))
}

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

type restrictedDialer struct {
	allowedHost  string
	allowPrivate bool
	resolver     Resolver
	dial         dialContextFunc
}

func (d restrictedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("restricted HTTP dial: %w", err)
	}
	normalizedHost := strings.ToLower(strings.TrimSuffix(host, "."))
	if normalizedHost != d.allowedHost {
		return nil, fmt.Errorf("%w: request host %q does not match %q", ErrAddressBlocked, host, d.allowedHost)
	}

	var addresses []netip.Addr
	if literal, parseErr := netip.ParseAddr(host); parseErr == nil {
		addresses = []netip.Addr{literal}
	} else {
		resolver := d.resolver
		if resolver == nil {
			resolver = net.DefaultResolver
		}
		addresses, err = resolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("restricted HTTP dial: resolve %s: %w", host, err)
		}
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("restricted HTTP dial: host %s resolved to no addresses", host)
	}

	dial := d.dial
	if dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	var lastDialErr error
	for _, candidate := range addresses {
		candidate = candidate.Unmap()
		if !PublicAddress(candidate) && (!d.allowPrivate || !privateAddress(candidate)) {
			continue
		}
		connection, dialErr := dial(ctx, network, net.JoinHostPort(candidate.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		lastDialErr = dialErr
	}
	if lastDialErr != nil {
		return nil, lastDialErr
	}
	return nil, fmt.Errorf("%w: %s resolved only to blocked addresses", ErrAddressBlocked, host)
}

// NewRestrictedClient pins all connections to endpoint's host, resolves and
// checks the address at dial time, ignores ambient proxy settings, and refuses
// redirects. allowPrivate is safe only when endpoint came from an
// operator-controlled exact allowlist.
func NewRestrictedClient(endpoint *url.URL, timeout time.Duration, allowPrivate bool) (*http.Client, error) {
	if endpoint == nil || endpoint.Hostname() == "" || endpoint.User != nil ||
		(endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("restricted HTTP endpoint must be an absolute HTTP(S) URL without userinfo")
	}
	transport, ok := CloneDefault().(*http.Transport)
	if !ok {
		return nil, errors.New("restricted HTTP client requires an HTTP transport")
	}
	transport.Proxy = nil
	transport.DialContext = restrictedDialer{
		allowedHost:  strings.ToLower(strings.TrimSuffix(endpoint.Hostname(), ".")),
		allowPrivate: allowPrivate,
	}.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}, nil
}
