package sip

import (
	"net"
	"testing"
	"time"
)

func TestTransactionKeyFromRequest(t *testing.T) {
	msg := NewRequest("INVITE", "sip:bob@example.com")
	msg.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient1")

	key := transactionKeyFromRequest(msg)
	if key != "INVITE|z9hG4bKclient1" {
		t.Fatalf("expected INVITE transaction key, got %q", key)
	}
}

func TestTransactionKeyFromResponse(t *testing.T) {
	resp := NewResponse(200, "OK")
	resp.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient1")
	resp.SetHeader("CSeq", "42 INVITE")

	key := transactionKeyFromResponse(resp)
	if key != "INVITE|z9hG4bKclient1" {
		t.Fatalf("expected response transaction key, got %q", key)
	}
}

func TestTransactionRouterRememberClone(t *testing.T) {
	router := newTransactionRouter(time.Minute)
	key := "INVITE|z9hG4bKclient1"
	original := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 5060}

	router.Remember(key, original)
	original.Port = 9999

	addr, ok := router.Lookup(key)
	if !ok {
		t.Fatalf("expected route to be remembered")
	}

	udp, ok := addr.(*net.UDPAddr)
	if !ok {
		t.Fatalf("expected UDP address, got %T", addr)
	}

	if udp.Port != 5060 {
		t.Fatalf("expected stored port to remain 5060, got %d", udp.Port)
	}
}

func TestTransactionRouterLookupExtendsTTL(t *testing.T) {
	ttl := 30 * time.Millisecond
	router := newTransactionRouter(ttl)
	key := "INVITE|z9hG4bKclient1"
	router.Remember(key, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 5060})

	router.mu.RLock()
	initialExpiry := router.routes[key].expires
	router.mu.RUnlock()

	time.Sleep(ttl / 2)

	if _, ok := router.Lookup(key); !ok {
		t.Fatalf("expected lookup to succeed before TTL expires")
	}

	router.mu.RLock()
	extendedExpiry := router.routes[key].expires
	router.mu.RUnlock()

	if !extendedExpiry.After(initialExpiry) {
		t.Fatalf("expected TTL to be extended, initial %v, extended %v", initialExpiry, extendedExpiry)
	}
}

func TestTransactionRouterExpires(t *testing.T) {
	ttl := 20 * time.Millisecond
	router := newTransactionRouter(ttl)
	key := "INVITE|z9hG4bKclient1"
	router.Remember(key, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 30), Port: 5060})

	time.Sleep(ttl + 10*time.Millisecond)

	if _, ok := router.Lookup(key); ok {
		t.Fatalf("expected route to expire after TTL")
	}
}
