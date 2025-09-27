package sip

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"xylitol4/sip/userdb"
)

// RegistrarStore exposes the credential lookup required by the registrar. It is
// satisfied by user directory backends such as userdb.SQLiteStore.
type RegistrarStore interface {
	Lookup(ctx context.Context, username, domain string) (*userdb.User, error)
}

// Registrar maintains client bindings registered via SIP REGISTER requests.
type Registrar struct {
	store RegistrarStore

	clock func() time.Time
	nonce func() string

	mu       sync.RWMutex
	bindings map[string][]registrationBinding
}

type registrationBinding struct {
	contact string
	expires time.Time
}

// Registration describes an active contact binding stored by the registrar.
type Registration struct {
	Contact string
	Expires time.Time
}

// NewRegistrar constructs a registrar backed by the provided store. A nil
// store is permitted but causes all REGISTER requests to fail with a 500
// response.
func NewRegistrar(store RegistrarStore) *Registrar {
	return &Registrar{
		store:    store,
		clock:    time.Now,
		nonce:    newNonce,
		bindings: make(map[string][]registrationBinding),
	}
}

func newNonce() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// handleRegister processes a REGISTER request. It returns the response that
// should be sent downstream together with a boolean indicating whether the
// message was fully handled by the registrar.
func (r *Registrar) handleRegister(ctx context.Context, req *Message) (*Message, bool) {
	if req == nil {
		return nil, true
	}

	username, domain, err := parseAddressOfRecord(req.GetHeader("To"))
	if err != nil {
		resp := registrarResponse(req, 400, "Bad Request")
		return resp, true
	}

	if r.store == nil {
		resp := registrarResponse(req, 500, "Server Internal Error")
		return resp, true
	}

	user, err := r.store.Lookup(ctx, username, domain)
	if err != nil {
		if errors.Is(err, userdb.ErrUserNotFound) {
			resp := registrarResponse(req, 404, "Not Found")
			return resp, true
		}
		resp := registrarResponse(req, 500, "Server Internal Error")
		return resp, true
	}

	authParams, ok := parseDigestAuthorization(req.GetHeader("Authorization"))
	if !ok {
		resp := registrarResponse(req, 401, "Unauthorized")
		challenge := fmt.Sprintf("Digest realm=\"%s\", nonce=\"%s\", algorithm=MD5, qop=\"auth\"", domain, r.nonce())
		resp.SetHeader("WWW-Authenticate", challenge)
		ensureToTag(resp)
		return resp, true
	}

	realm := authParams["realm"]
	if realm == "" {
		realm = domain
	}
	if !strings.EqualFold(authParams["username"], user.Username) || !strings.EqualFold(realm, user.Domain) {
		resp := registrarResponse(req, 403, "Forbidden")
		ensureToTag(resp)
		return resp, true
	}

	if err := verifyDigest(authParams, req, user, realm); err != nil {
		resp := registrarResponse(req, 403, "Forbidden")
		ensureToTag(resp)
		return resp, true
	}

	bindings, regErr := r.applyRegistration(registrarKey(user.Username, user.Domain), req)
	if regErr != nil {
		resp := registrarResponse(req, regErr.status, regErr.reason)
		ensureToTag(resp)
		return resp, true
	}

	resp := registrarResponse(req, 200, "OK")
	if len(bindings) > 0 {
		now := r.clock()
		contacts := make([]string, 0, len(bindings))
		for _, binding := range bindings {
			remaining := int(binding.expires.Sub(now) / time.Second)
			if remaining < 0 {
				remaining = 0
			}
			contacts = append(contacts, withContactExpires(binding.contact, remaining))
		}
		resp.SetHeader("Contact", contacts...)
	}
	ensureToTag(resp)
	return resp, true
}

type registrarError struct {
	status int
	reason string
}

func (e *registrarError) Error() string {
	return fmt.Sprintf("registrar error %d: %s", e.status, e.reason)
}

func (r *Registrar) applyRegistration(key string, req *Message) ([]registrationBinding, *registrarError) {
	now := r.clock()

	r.mu.Lock()
	defer r.mu.Unlock()

	existing := r.bindings[key]
	filtered := make([]registrationBinding, 0, len(existing))
	for _, binding := range existing {
		if binding.expires.After(now) {
			filtered = append(filtered, binding)
		}
	}

	contacts := expandContactValues(req.HeaderValues("Contact"))
	defaultExpires := parseExpires(req.GetHeader("Expires"))

	if len(contacts) == 0 {
		r.bindings[key] = filtered
		return filtered, nil
	}

	if len(contacts) == 1 && strings.EqualFold(strings.TrimSpace(contacts[0]), "*") {
		if defaultExpires != 0 {
			return nil, &registrarError{status: 400, reason: "Invalid wildcard contact"}
		}
		delete(r.bindings, key)
		return nil, nil
	}

	result := filtered
	for _, raw := range contacts {
		address := contactAddress(raw)
		if address == "" {
			return nil, &registrarError{status: 400, reason: "Invalid Contact header"}
		}
		expires := parseExpires(GetHeaderParam(raw, "expires"))
		if expires < 0 {
			expires = defaultExpires
		}
		if expires < 0 {
			expires = 3600
		}
		result = removeBindingByAddress(result, address)
		if expires == 0 {
			continue
		}
		normalized := normalizeContact(raw, expires)
		binding := registrationBinding{
			contact: normalized,
			expires: now.Add(time.Duration(expires) * time.Second),
		}
		result = append(result, binding)
	}

	r.bindings[key] = result
	return result, nil
}

// BindingsFor returns active registrations for the provided username and domain.
func (r *Registrar) BindingsFor(username, domain string) []Registration {
	if r == nil {
		return nil
	}
	key := registrarKey(username, domain)
	now := r.clock()

	r.mu.Lock()
	defer r.mu.Unlock()

	existing := r.bindings[key]
	filtered := make([]registrationBinding, 0, len(existing))
	for _, binding := range existing {
		if binding.expires.After(now) {
			filtered = append(filtered, binding)
		}
	}
	if len(filtered) == 0 {
		delete(r.bindings, key)
		return nil
	}
	r.bindings[key] = filtered
	out := make([]Registration, len(filtered))
	for i, binding := range filtered {
		out[i] = Registration{Contact: binding.contact, Expires: binding.expires}
	}
	return out
}

func registrarKey(username, domain string) string {
	return strings.ToLower(strings.TrimSpace(username)) + "@" + strings.ToLower(strings.TrimSpace(domain))
}

func registrarResponse(req *Message, status int, reason string) *Message {
	resp := NewResponse(status, reason)
	if req != nil {
		CopyHeaders(resp, req, "Via", "From", "To", "Call-ID", "CSeq")
	}
	resp.SetHeader("Content-Length", "0")
	return resp
}

func ensureToTag(resp *Message) {
	if resp == nil {
		return
	}
	to := resp.GetHeader("To")
	if to == "" {
		return
	}
	lower := strings.ToLower(to)
	if strings.Contains(lower, ";tag=") {
		return
	}
	resp.SetHeader("To", replaceHeaderParam(to, "tag", newTag()))
}

func parseAddressOfRecord(to string) (string, string, error) {
	to = strings.TrimSpace(to)
	if to == "" {
		return "", "", fmt.Errorf("registrar: missing To header")
	}
	if idx := strings.Index(to, "<"); idx != -1 {
		if end := strings.Index(to[idx:], ">"); end != -1 {
			to = to[idx+1 : idx+end]
		}
	}
	if idx := strings.Index(to, ">"); idx != -1 {
		to = to[:idx]
	}
	to = strings.TrimSpace(to)
	if strings.HasPrefix(strings.ToLower(to), "sip:") {
		to = to[4:]
	}
	if idx := strings.Index(to, ";"); idx != -1 {
		to = to[:idx]
	}
	parts := strings.SplitN(to, "@", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("registrar: invalid address of record")
	}
	user := strings.TrimSpace(parts[0])
	domain := strings.TrimSpace(parts[1])
	if user == "" || domain == "" {
		return "", "", fmt.Errorf("registrar: invalid address of record")
	}
	return user, domain, nil
}

func parseDigestAuthorization(header string) (map[string]string, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return nil, false
	}
	if !strings.HasPrefix(strings.ToLower(header), "digest ") {
		return nil, false
	}
	params := header[len("Digest "):]
	segments := splitAuthParams(params)
	values := make(map[string]string, len(segments))
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		kv := strings.SplitN(segment, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(kv[0]))
		value := strings.TrimSpace(kv[1])
		value = strings.Trim(value, "\"")
		values[key] = value
	}
	if len(values) == 0 {
		return nil, false
	}
	return values, true
}

func splitAuthParams(input string) []string {
	var (
		parts []string
		buf   strings.Builder
		inQ   bool
	)
	for _, r := range input {
		switch r {
		case '"':
			inQ = !inQ
			buf.WriteRune(r)
		case ',':
			if inQ {
				buf.WriteRune(r)
				continue
			}
			parts = append(parts, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
}

func verifyDigest(params map[string]string, req *Message, user *userdb.User, realm string) error {
	if req == nil || user == nil {
		return fmt.Errorf("missing data")
	}
	algorithm := params["algorithm"]
	if algorithm != "" && !strings.EqualFold(algorithm, "md5") {
		return fmt.Errorf("unsupported algorithm %s", algorithm)
	}
	nonce := params["nonce"]
	response := params["response"]
	if nonce == "" || response == "" {
		return fmt.Errorf("missing nonce or response")
	}
	uri := params["uri"]
	if uri == "" {
		uri = req.RequestURI
	}
	ha1 := userdb.ComputeHA1(user.Username, realm, user.PasswordHash)
	if ha1 == "" {
		return fmt.Errorf("missing credentials")
	}
	ha2 := md5Hex(fmt.Sprintf("%s:%s", strings.ToUpper(req.Method), uri))
	qop := strings.ToLower(params["qop"])
	var expected string
	switch qop {
	case "":
		expected = md5Hex(fmt.Sprintf("%s:%s:%s", ha1, nonce, ha2))
	case "auth":
		nc := params["nc"]
		cnonce := params["cnonce"]
		if nc == "" || cnonce == "" {
			return fmt.Errorf("missing nonce counters")
		}
		expected = md5Hex(fmt.Sprintf("%s:%s:%s:%s:%s:%s", ha1, nonce, nc, cnonce, qop, ha2))
	default:
		return fmt.Errorf("unsupported qop %s", qop)
	}
	if !strings.EqualFold(expected, response) {
		return fmt.Errorf("digest mismatch")
	}
	return nil
}

func md5Hex(input string) string {
	sum := md5.Sum([]byte(input))
	return hex.EncodeToString(sum[:])
}

func isHex(value string) bool {
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func expandContactValues(values []string) []string {
	var contacts []string
	for _, value := range values {
		parts := splitContactList(value)
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				contacts = append(contacts, trimmed)
			}
		}
	}
	return contacts
}

func splitContactList(value string) []string {
	var (
		parts []string
		buf   strings.Builder
		inQ   bool
		depth int
	)
	for _, r := range value {
		switch r {
		case '"':
			inQ = !inQ
			buf.WriteRune(r)
		case '<':
			if !inQ {
				depth++
			}
			buf.WriteRune(r)
		case '>':
			if !inQ && depth > 0 {
				depth--
			}
			buf.WriteRune(r)
		case ',':
			if inQ || depth > 0 {
				buf.WriteRune(r)
				continue
			}
			parts = append(parts, buf.String())
			buf.Reset()
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
}

func contactAddress(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parts := strings.SplitN(value, ";", 2)
	return strings.TrimSpace(parts[0])
}

func contactKey(value string) string {
	return strings.ToLower(contactAddress(value))
}

func removeBindingByAddress(bindings []registrationBinding, address string) []registrationBinding {
	key := contactKey(address)
	if key == "" {
		return bindings
	}
	filtered := make([]registrationBinding, 0, len(bindings))
	for _, binding := range bindings {
		if contactKey(binding.contact) == key {
			continue
		}
		filtered = append(filtered, binding)
	}
	return filtered
}

func normalizeContact(value string, expires int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	segments := strings.Split(value, ";")
	base := strings.TrimSpace(segments[0])
	params := make([]string, 0, len(segments))
	for _, segment := range segments[1:] {
		trimmed := strings.TrimSpace(segment)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "expires=") {
			continue
		}
		params = append(params, trimmed)
	}
	params = append(params, fmt.Sprintf("expires=%d", expires))
	return strings.Join(append([]string{base}, params...), ";")
}

func withContactExpires(value string, expires int) string {
	if expires < 0 {
		expires = 0
	}
	return normalizeContact(value, expires)
}

func parseExpires(raw string) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return -1
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return -1
	}
	if value < 0 {
		return 0
	}
	return value
}

func newTag() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
