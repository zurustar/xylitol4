package sip

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"xylitol4/sip/userdb"
)

type memoryRegistrarStore struct {
	users map[string]*userdb.User
}

func newMemoryStore() *memoryRegistrarStore {
	return &memoryRegistrarStore{users: make(map[string]*userdb.User)}
}

func (m *memoryRegistrarStore) add(user *userdb.User) {
	if user == nil {
		return
	}
	key := registrarKey(user.Username, user.Domain)
	m.users[key] = user
}

func (m *memoryRegistrarStore) Lookup(ctx context.Context, username, domain string) (*userdb.User, error) {
	key := registrarKey(username, domain)
	user, ok := m.users[key]
	if !ok {
		return nil, userdb.ErrUserNotFound
	}
	clone := *user
	return &clone, nil
}

func TestRegistrarChallengesWithoutAuth(t *testing.T) {
	store := newMemoryStore()
	store.add(&userdb.User{Username: "alice", Domain: "example.com", PasswordHash: md5Hex("alice:example.com:secret")})
	registrar := NewRegistrar(store)
	registrar.clock = func() time.Time { return time.Unix(1_700_000_000, 0) }

	req := newRegisterRequest()
	resp, handled := registrar.handleRegister(context.Background(), req)
	if !handled {
		t.Fatalf("expected registrar to handle REGISTER locally")
	}
	if resp == nil || resp.StatusCode != 401 {
		t.Fatalf("expected 401 challenge, got %v", resp)
	}
	challenge := resp.GetHeader("WWW-Authenticate")
	if !strings.Contains(challenge, "Digest realm=\"example.com\"") {
		t.Fatalf("expected WWW-Authenticate header, got %q", challenge)
	}
	if to := strings.ToLower(resp.GetHeader("To")); !strings.Contains(to, "tag=") {
		t.Fatalf("expected To header to include tag, got %q", resp.GetHeader("To"))
	}
}

func TestRegistrarRejectsUnknownUser(t *testing.T) {
	registrar := NewRegistrar(newMemoryStore())
	req := newRegisterRequest()
	resp, handled := registrar.handleRegister(context.Background(), req)
	if !handled {
		t.Fatalf("expected registrar to handle REGISTER")
	}
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for unknown user, got %d", resp.StatusCode)
	}
}

func TestRegistrarAcceptsValidDigest(t *testing.T) {
	password := "supersecret"
	realm := "example.com"
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", "alice", realm, password))
	store := newMemoryStore()
	store.add(&userdb.User{Username: "alice", Domain: realm, PasswordHash: ha1})
	registrar := NewRegistrar(store)
	now := time.Unix(1_700_000_000, 0)
	registrar.clock = func() time.Time { return now }

	req := newRegisterRequest()
	resp, _ := registrar.handleRegister(context.Background(), req)
	nonce := extractNonce(t, resp)

	authReq := newRegisterRequest()
	authReq.SetHeader("Authorization", buildAuthorization("alice", realm, ha1, nonce, 1, "cnonce-value", authReq.Method, authReq.RequestURI))

	resp, handled := registrar.handleRegister(context.Background(), authReq)
	if !handled {
		t.Fatalf("expected registrar to accept REGISTER")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	contacts := resp.HeaderValues("Contact")
	if len(contacts) != 1 || !strings.Contains(contacts[0], "<sip:alice@client.example.com>") {
		t.Fatalf("unexpected contact header %v", contacts)
	}

	bindings := registrar.BindingsFor("alice", realm)
	if len(bindings) != 1 {
		t.Fatalf("expected one binding, got %d", len(bindings))
	}
	if !strings.Contains(bindings[0].Contact, "<sip:alice@client.example.com>") {
		t.Fatalf("unexpected stored contact %q", bindings[0].Contact)
	}
	if !bindings[0].Expires.After(now) {
		t.Fatalf("expected expiry after now, got %v", bindings[0].Expires)
	}
}

func TestRegistrarRejectsInvalidDigest(t *testing.T) {
	realm := "example.com"
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", "alice", realm, "secret"))
	store := newMemoryStore()
	store.add(&userdb.User{Username: "alice", Domain: realm, PasswordHash: ha1})
	registrar := NewRegistrar(store)

	challengeResp, _ := registrar.handleRegister(context.Background(), newRegisterRequest())
	nonce := extractNonce(t, challengeResp)

	badReq := newRegisterRequest()
	// modify password to produce incorrect response
	badReq.SetHeader("Authorization", buildAuthorization("alice", realm, md5Hex("alice:"+realm+":wrong"), nonce, 1, "badcnonce", badReq.Method, badReq.RequestURI))

	resp, handled := registrar.handleRegister(context.Background(), badReq)
	if !handled {
		t.Fatalf("expected registrar to process digest")
	}
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 for invalid credentials, got %d", resp.StatusCode)
	}
}

func TestRegistrarDeregistersWildcard(t *testing.T) {
	password := "supersecret"
	realm := "example.com"
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", "alice", realm, password))
	store := newMemoryStore()
	store.add(&userdb.User{Username: "alice", Domain: realm, PasswordHash: ha1})
	registrar := NewRegistrar(store)

	// Initial registration
	challengeResp, _ := registrar.handleRegister(context.Background(), newRegisterRequest())
	nonce := extractNonce(t, challengeResp)

	registerReq := newRegisterRequest()
	registerReq.SetHeader("Authorization", buildAuthorization("alice", realm, ha1, nonce, 1, "cnonce", registerReq.Method, registerReq.RequestURI))
	if resp, _ := registrar.handleRegister(context.Background(), registerReq); resp.StatusCode != 200 {
		t.Fatalf("expected successful registration, got %d", resp.StatusCode)
	}

	bindings := registrar.BindingsFor("alice", realm)
	if len(bindings) != 1 {
		t.Fatalf("expected binding to be stored")
	}

	// Deregistration using wildcard contact and expires=0
	deregReq := NewRequest("REGISTER", "sip:"+realm)
	deregReq.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient")
	deregReq.SetHeader("From", "<sip:alice@example.com>;tag=1928301774")
	deregReq.SetHeader("To", "<sip:alice@example.com>")
	deregReq.SetHeader("Call-ID", "reg-test")
	deregReq.SetHeader("CSeq", "2 REGISTER")
	deregReq.SetHeader("Contact", "*")
	deregReq.SetHeader("Expires", "0")
	deregReq.SetHeader("Content-Length", "0")
	deregReq.SetHeader("Authorization", buildAuthorization("alice", realm, ha1, nonce, 2, "cnonce", deregReq.Method, deregReq.RequestURI))

	resp, handled := registrar.handleRegister(context.Background(), deregReq)
	if !handled {
		t.Fatalf("expected registrar to handle deregistration")
	}
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 after deregistration, got %d", resp.StatusCode)
	}
	if bindings := registrar.BindingsFor("alice", realm); len(bindings) != 0 {
		t.Fatalf("expected bindings to be cleared, got %v", bindings)
	}
}

func TestProxyHandlesRegisterLocally(t *testing.T) {
	realm := "example.com"
	password := "secret"
	ha1 := md5Hex(fmt.Sprintf("%s:%s:%s", "alice", realm, password))
	store := newMemoryStore()
	store.add(&userdb.User{Username: "alice", Domain: realm, PasswordHash: ha1})
	registrar := NewRegistrar(store)
	proxy := NewProxy(WithRegistrar(registrar))
	t.Cleanup(proxy.Stop)

	initial := newRegisterRequest()
	proxy.SendFromClient(initial)
	resp, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok || resp.StatusCode != 401 {
		t.Fatalf("expected 401 challenge, got %v", resp)
	}
	if _, ok := proxy.NextToServer(50 * time.Millisecond); ok {
		t.Fatalf("REGISTER should not be forwarded upstream")
	}

	nonce := extractNonce(t, resp)

	authorised := newRegisterRequest()
	authorised.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient-auth")
	authorised.SetHeader("CSeq", "2 REGISTER")
	authorised.SetHeader("Authorization", buildAuthorization("alice", realm, ha1, nonce, 1, "cnonce", authorised.Method, authorised.RequestURI))
	proxy.SendFromClient(authorised)
	okResp, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok || okResp.StatusCode != 200 {
		t.Fatalf("expected 200 OK, got %v", okResp)
	}
	if _, ok := proxy.NextToServer(50 * time.Millisecond); ok {
		t.Fatalf("authorised REGISTER should not traverse upstream")
	}
}

func newRegisterRequest() *Message {
	req := NewRequest("REGISTER", "sip:example.com")
	req.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient")
	req.SetHeader("From", "<sip:alice@example.com>;tag=1928301774")
	req.SetHeader("To", "<sip:alice@example.com>")
	req.SetHeader("Call-ID", "reg-call-id")
	req.SetHeader("CSeq", "1 REGISTER")
	req.SetHeader("Contact", "<sip:alice@client.example.com>;expires=600")
	req.SetHeader("Max-Forwards", "70")
	req.SetHeader("Content-Length", "0")
	return req
}

func extractNonce(t *testing.T, resp *Message) string {
	t.Helper()
	if resp == nil {
		t.Fatalf("response is nil")
	}
	params, ok := parseDigestAuthorization(resp.GetHeader("WWW-Authenticate"))
	if !ok {
		t.Fatalf("failed to parse challenge: %q", resp.GetHeader("WWW-Authenticate"))
	}
	nonce := params["nonce"]
	if nonce == "" {
		t.Fatalf("challenge missing nonce: %v", params)
	}
	return nonce
}

func buildAuthorization(username, realm, ha1, nonce string, nc int, cnonce, method, uri string) string {
	if method == "" {
		method = "REGISTER"
	}
	if uri == "" {
		uri = "sip:" + realm
	}
	ncStr := fmt.Sprintf("%08x", nc)
	ha2 := md5Hex(fmt.Sprintf("%s:%s", strings.ToUpper(method), uri))
	response := md5Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s", ha1, nonce, ncStr, cnonce, "auth", ha2))
	return fmt.Sprintf("Digest username=\"%s\", realm=\"%s\", nonce=\"%s\", uri=\"%s\", response=\"%s\", algorithm=MD5, qop=auth, nc=%s, cnonce=\"%s\"", username, realm, nonce, uri, response, ncStr, cnonce)
}
