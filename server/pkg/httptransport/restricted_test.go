package httptransport

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

type staticResolver struct {
	addresses []netip.Addr
}

func (r staticResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return r.addresses, nil
}

func TestPublicAddressPolicy(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.0.1",
		"100.64.0.1",
		"169.254.169.254",
		"::1",
		"fc00::1",
		"fe80::1",
		"::ffff:127.0.0.1",
		"64:ff9b::a9fe:a9fe",
		"2002:7f00:1::1",
	} {
		if PublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("PublicAddress(%s) allowed a protected address", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2001:4860:4860::8888"} {
		if !PublicAddress(netip.MustParseAddr(raw)) {
			t.Errorf("PublicAddress(%s) rejected a public address", raw)
		}
	}
}

func TestRestrictedDialerChecksResolvedAddressBeforeDial(t *testing.T) {
	dialed := false
	dialer := restrictedDialer{
		allowedHost: "provider.example",
		resolver: staticResolver{addresses: []netip.Addr{
			netip.MustParseAddr("169.254.169.254"),
		}},
		dial: func(context.Context, string, string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("unexpected dial")
		},
	}
	_, err := dialer.DialContext(context.Background(), "tcp", "provider.example:443")
	if !errors.Is(err, ErrAddressBlocked) {
		t.Fatalf("DialContext error = %v, want ErrAddressBlocked", err)
	}
	if dialed {
		t.Fatal("blocked address reached the network dialer")
	}
}

func TestRestrictedDialerAllowsPublicAddress(t *testing.T) {
	wantErr := errors.New("dial attempted")
	var dialedAddress string
	dialer := restrictedDialer{
		allowedHost: "provider.example",
		resolver: staticResolver{addresses: []netip.Addr{
			netip.MustParseAddr("1.1.1.1"),
		}},
		dial: func(_ context.Context, _, address string) (net.Conn, error) {
			dialedAddress = address
			return nil, wantErr
		},
	}
	_, err := dialer.DialContext(context.Background(), "tcp", "provider.example:443")
	if !errors.Is(err, wantErr) {
		t.Fatalf("DialContext error = %v, want dial error", err)
	}
	if dialedAddress != "1.1.1.1:443" {
		t.Fatalf("dialed address = %q, want checked literal", dialedAddress)
	}
}

func TestRestrictedDialerPrivateOptInRemainsHostPinned(t *testing.T) {
	wantErr := errors.New("dial attempted")
	dialer := restrictedDialer{
		allowedHost:  "gitlab.internal",
		allowPrivate: true,
		resolver: staticResolver{addresses: []netip.Addr{
			netip.MustParseAddr("10.0.0.8"),
		}},
		dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, wantErr
		},
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", "gitlab.internal:443"); !errors.Is(err, wantErr) {
		t.Fatalf("private allowlist error = %v, want dial attempt", err)
	}
	if _, err := dialer.DialContext(context.Background(), "tcp", "other.internal:443"); !errors.Is(err, ErrAddressBlocked) {
		t.Fatalf("changed host error = %v, want ErrAddressBlocked", err)
	}

	dialer.resolver = staticResolver{addresses: []netip.Addr{
		netip.MustParseAddr("64:ff9b::a9fe:a9fe"),
	}}
	if _, err := dialer.DialContext(context.Background(), "tcp", "gitlab.internal:443"); !errors.Is(err, ErrAddressBlocked) {
		t.Fatalf("translation address error = %v, want ErrAddressBlocked", err)
	}
}
