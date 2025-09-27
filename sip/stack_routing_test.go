package sip

import (
	"net"
	"testing"
	"time"

	"xylitol4/sip/userdb"
)

func TestSelectUpstreamTargetUsesRegistrarBinding(t *testing.T) {
	registrar := NewRegistrar(nil)
	now := time.Now()
	registrar.clock = func() time.Time { return now }
	registrar.bindings[registrarKey("bob", "example.com")] = []registrationBinding{{
		contact: "<sip:bob@192.0.2.55:5070>",
		expires: now.Add(time.Hour),
	}}

	stack := &SIPStack{
		registrar:      registrar,
		managedDomains: map[string]struct{}{"example.com": {}},
		directory:      make(map[string]userdb.User),
		upstreamAddr:   &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 5060},
	}

	req := NewRequest("INVITE", "sip:bob@example.com")
	addr, err := stack.selectUpstreamTarget(req)
	if err != nil {
		t.Fatalf("selectUpstreamTarget returned error: %v", err)
	}
	if addr == nil {
		t.Fatalf("expected address from registrar binding")
	}
	if got := addr.String(); got != "192.0.2.55:5070" {
		t.Fatalf("unexpected registrar target: %s", got)
	}
}

func TestSelectUpstreamTargetFallsBackToDirectory(t *testing.T) {
	stack := &SIPStack{
		managedDomains: map[string]struct{}{"example.com": {}},
		directory: map[string]userdb.User{
			registrarKey("bob", "example.com"): {
				Username:   "bob",
				Domain:     "example.com",
				ContactURI: "sip:bob@198.51.100.10:5090",
			},
		},
		upstreamAddr: &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 5060},
	}

	req := NewRequest("INVITE", "sip:bob@example.com")
	addr, err := stack.selectUpstreamTarget(req)
	if err != nil {
		t.Fatalf("selectUpstreamTarget returned error: %v", err)
	}
	if addr == nil {
		t.Fatalf("expected directory-based address")
	}
	if got := addr.String(); got != "198.51.100.10:5090" {
		t.Fatalf("unexpected directory target: %s", got)
	}
}

func TestSelectUpstreamTargetResolvesRequestURI(t *testing.T) {
	stack := &SIPStack{
		managedDomains: make(map[string]struct{}),
		upstreamAddr:   &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 5060},
	}

	req := NewRequest("INVITE", "sip:bob@203.0.113.5:5090")
	addr, err := stack.selectUpstreamTarget(req)
	if err != nil {
		t.Fatalf("selectUpstreamTarget returned error: %v", err)
	}
	if addr == nil {
		t.Fatalf("expected resolved address")
	}
	if got := addr.String(); got != "203.0.113.5:5090" {
		t.Fatalf("unexpected resolved target: %s", got)
	}
}

func TestSelectUpstreamTargetFallsBackToDefault(t *testing.T) {
	fallback := &net.UDPAddr{IP: net.IPv4(198, 51, 100, 1), Port: 5060}
	stack := &SIPStack{
		managedDomains: make(map[string]struct{}),
		upstreamAddr:   fallback,
	}

	req := NewRequest("INVITE", "sip:bob@")
	addr, err := stack.selectUpstreamTarget(req)
	if err != nil {
		t.Fatalf("selectUpstreamTarget returned error: %v", err)
	}
	if addr == nil {
		t.Fatalf("expected fallback address")
	}
	if got := addr.String(); got != fallback.String() {
		t.Fatalf("expected fallback %s, got %s", fallback, got)
	}
}

func TestSelectUpstreamTargetErrorsWithoutRoute(t *testing.T) {
	stack := &SIPStack{}
	req := NewRequest("INVITE", "sip:bob@")
	if _, err := stack.selectUpstreamTarget(req); err == nil {
		t.Fatalf("expected error when no route is available")
	}
}
