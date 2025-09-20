package sip

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server implements a simple stateful SIP server with session timer support.
type Server struct {
	mu                     sync.Mutex
	dialogs                map[string]*dialog
	defaultSessionInterval time.Duration
	contact                string
	now                    func() time.Time
}

// DialogState represents an exported snapshot of an active dialog.
type DialogState struct {
	CallID          string
	LocalTag        string
	RemoteTag       string
	SessionInterval time.Duration
	Refresher       string
	LastUpdated     time.Time
}

// Expiration returns the point in time when the dialog will expire.
func (d DialogState) Expiration() time.Time {
	if d.SessionInterval <= 0 {
		return time.Time{}
	}
	return d.LastUpdated.Add(d.SessionInterval)
}

// Remaining reports the remaining time until the dialog expires.
func (d DialogState) Remaining(now time.Time) time.Duration {
	if d.SessionInterval <= 0 {
		return 0
	}
	return d.Expiration().Sub(now)
}

type dialog struct {
	CallID          string
	LocalTag        string
	RemoteTag       string
	SessionInterval time.Duration
	Refresher       string
	LastUpdated     time.Time
}

type Option func(*Server)

// WithDefaultSessionInterval configures the default session interval used when
// a request does not supply Session-Expires.
func WithDefaultSessionInterval(interval time.Duration) Option {
	return func(s *Server) {
		if interval > 0 {
			s.defaultSessionInterval = interval
		}
	}
}

// WithContact configures the Contact header added to responses.
func WithContact(contact string) Option {
	return func(s *Server) {
		s.contact = contact
	}
}

// WithClock overrides the clock used by the server. Mainly intended for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

// NewServer creates a new SIP server.
func NewServer(opts ...Option) *Server {
	server := &Server{
		dialogs:                make(map[string]*dialog),
		defaultSessionInterval: 30 * time.Minute,
		contact:                "<sip:server@localhost>",
		now:                    time.Now,
	}
	for _, opt := range opts {
		opt(server)
	}
	if server.defaultSessionInterval <= 0 {
		server.defaultSessionInterval = 30 * time.Minute
	}
	if server.contact == "" {
		server.contact = "<sip:server@localhost>"
	}
	if server.now == nil {
		server.now = time.Now
	}
	return server
}

// Handle decodes a raw SIP message and returns the responses in wire format.
func (s *Server) Handle(raw string) ([]string, error) {
	msg, err := ParseMessage(raw)
	if err != nil {
		return nil, err
	}
	responses, err := s.HandleMessage(msg)
	if err != nil {
		return nil, err
	}
	encoded := make([]string, 0, len(responses))
	for _, resp := range responses {
		encoded = append(encoded, resp.String())
	}
	return encoded, nil
}

// HandleMessage processes a parsed SIP request and returns the generated responses.
func (s *Server) HandleMessage(msg *Message) ([]*Message, error) {
	if msg == nil {
		return nil, ErrInvalidMessage
	}
	if !msg.IsRequest() {
		return nil, errors.New("SIP server only handles requests")
	}
	switch strings.ToUpper(msg.Method) {
	case "INVITE":
		return s.handleInvite(msg)
	case "ACK":
		return nil, nil
	case "BYE":
		return s.handleBye(msg)
	case "UPDATE":
		return s.handleUpdate(msg)
	case "OPTIONS":
		return []*Message{s.buildResponse(msg, 200, "OK")}, nil
	default:
		return []*Message{s.buildResponse(msg, 501, "Not Implemented")}, nil
	}
}

func (s *Server) handleInvite(req *Message) ([]*Message, error) {
	callID := strings.TrimSpace(req.GetHeader("Call-ID"))
	if callID == "" {
		return []*Message{s.buildResponse(req, 400, "Missing Call-ID")}, nil
	}
	from := req.GetHeader("From")
	fromTag := GetHeaderParam(from, "tag")
	if fromTag == "" {
		return []*Message{s.buildResponse(req, 400, "Missing From tag")}, nil
	}
	to := req.GetHeader("To")
	toTag := GetHeaderParam(to, "tag")

	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	key := dialogKey(callID, fromTag, toTag)
	dlg, exists := s.dialogs[key]

	if !exists {
		if toTag == "" {
			toTag = s.generateTagLocked()
		}
		key = dialogKey(callID, fromTag, toTag)
		dlg = &dialog{
			CallID:          callID,
			LocalTag:        toTag,
			RemoteTag:       fromTag,
			SessionInterval: s.defaultSessionInterval,
			Refresher:       "uas",
			LastUpdated:     now,
		}
		s.dialogs[key] = dlg
	}

	interval, refresher, err := parseSessionExpires(req.GetHeader("Session-Expires"))
	if err != nil {
		return []*Message{s.buildResponse(req, 400, "Bad Session-Expires")}, nil
	}

	if interval <= 0 {
		minSE := parseMinSE(req.GetHeader("Min-SE"))
		if minSE > 0 && minSE > s.defaultSessionInterval {
			interval = minSE
		} else if dlg.SessionInterval > 0 {
			interval = dlg.SessionInterval
		} else {
			interval = s.defaultSessionInterval
		}
	}

	if interval > 0 {
		dlg.SessionInterval = interval
	}
	if refresher != "" {
		dlg.Refresher = refresher
	}
	dlg.LastUpdated = now

	resp := s.buildResponse(req, 200, "OK")
	resp.SetHeader("To", ensureTagPresent(to, dlg.LocalTag))
	resp.SetHeader("Contact", s.contact)
	resp.SetHeader("Allow", "INVITE, ACK, BYE, UPDATE, OPTIONS")
	resp.SetHeader("Supported", "timer")
	resp.SetHeader("Require", "timer")
	resp.SetHeader("Session-Expires", formatSessionExpires(dlg.SessionInterval, dlg.Refresher))

	return []*Message{resp}, nil
}

func (s *Server) handleBye(req *Message) ([]*Message, error) {
	callID := strings.TrimSpace(req.GetHeader("Call-ID"))
	if callID == "" {
		return []*Message{s.buildResponse(req, 400, "Missing Call-ID")}, nil
	}
	fromTag := GetHeaderParam(req.GetHeader("From"), "tag")
	toTag := GetHeaderParam(req.GetHeader("To"), "tag")

	s.mu.Lock()
	key := dialogKey(callID, fromTag, toTag)
	dlg, ok := s.dialogs[key]
	if !ok {
		// Try swapped tags in case BYE originates from the server side.
		key = dialogKey(callID, toTag, fromTag)
		dlg, ok = s.dialogs[key]
	}
	if ok {
		delete(s.dialogs, key)
	}
	s.mu.Unlock()

	if !ok {
		return []*Message{s.buildResponse(req, 481, "Call/Transaction Does Not Exist")}, nil
	}

	resp := s.buildResponse(req, 200, "OK")
	resp.SetHeader("To", ensureTagPresent(req.GetHeader("To"), dlg.LocalTag))
	resp.SetHeader("Contact", s.contact)
	resp.SetHeader("Allow", "INVITE, ACK, BYE, UPDATE, OPTIONS")
	return []*Message{resp}, nil
}

func (s *Server) handleUpdate(req *Message) ([]*Message, error) {
	callID := strings.TrimSpace(req.GetHeader("Call-ID"))
	if callID == "" {
		return []*Message{s.buildResponse(req, 400, "Missing Call-ID")}, nil
	}
	fromTag := GetHeaderParam(req.GetHeader("From"), "tag")
	toTag := GetHeaderParam(req.GetHeader("To"), "tag")

	s.mu.Lock()
	defer s.mu.Unlock()

	key := dialogKey(callID, fromTag, toTag)
	dlg, ok := s.dialogs[key]
	if !ok {
		key = dialogKey(callID, toTag, fromTag)
		dlg, ok = s.dialogs[key]
	}
	if !ok {
		return []*Message{s.buildResponse(req, 481, "Call/Transaction Does Not Exist")}, nil
	}

	interval, refresher, err := parseSessionExpires(req.GetHeader("Session-Expires"))
	if err != nil {
		return []*Message{s.buildResponse(req, 400, "Bad Session-Expires")}, nil
	}
	if interval <= 0 {
		interval = dlg.SessionInterval
	}
	if interval > 0 {
		dlg.SessionInterval = interval
	}
	if refresher != "" {
		dlg.Refresher = refresher
	}
	dlg.LastUpdated = s.now()

	resp := s.buildResponse(req, 200, "OK")
	resp.SetHeader("To", ensureTagPresent(req.GetHeader("To"), dlg.LocalTag))
	resp.SetHeader("Contact", s.contact)
	resp.SetHeader("Allow", "INVITE, ACK, BYE, UPDATE, OPTIONS")
	resp.SetHeader("Supported", "timer")
	resp.SetHeader("Require", "timer")
	resp.SetHeader("Session-Expires", formatSessionExpires(dlg.SessionInterval, dlg.Refresher))
	return []*Message{resp}, nil
}

// buildResponse creates a response sharing the essential headers with the request.
func (s *Server) buildResponse(req *Message, status int, reason string) *Message {
	resp := NewResponse(status, reason)
	if req == nil {
		resp.SetHeader("Content-Length", "0")
		return resp
	}
	CopyHeaders(resp, req, "Via", "From", "Call-ID", "CSeq")
	if to := req.GetHeader("To"); to != "" {
		resp.SetHeader("To", to)
	}
	if len(resp.Body) == 0 {
		resp.Body = ""
	}
	resp.EnsureContentLength()
	return resp
}

// ServeUDP starts a UDP listener and handles requests synchronously.
func (s *Server) ServeUDP(address string) error {
	if address == "" {
		address = ":5060"
	}
	conn, err := net.ListenPacket("udp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	buffer := make([]byte, 65535)
	for {
		n, addr, err := conn.ReadFrom(buffer)
		if err != nil {
			return err
		}
		reqData := string(buffer[:n])
		responses, err := s.Handle(reqData)
		if err != nil {
			continue
		}
		for _, resp := range responses {
			_, _ = conn.WriteTo([]byte(resp), addr)
		}
	}
}

// ActiveDialogs returns a snapshot of all dialogs.
func (s *Server) ActiveDialogs() []DialogState {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make([]DialogState, 0, len(s.dialogs))
	for _, dlg := range s.dialogs {
		states = append(states, dlg.snapshot())
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].CallID == states[j].CallID {
			if states[i].LocalTag == states[j].LocalTag {
				return states[i].RemoteTag < states[j].RemoteTag
			}
			return states[i].LocalTag < states[j].LocalTag
		}
		return states[i].CallID < states[j].CallID
	})
	return states
}

// DialogState looks up a dialog using the supplied identifiers.
func (s *Server) DialogState(callID, tagA, tagB string) (DialogState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := dialogKey(callID, tagA, tagB)
	if dlg, ok := s.dialogs[key]; ok {
		return dlg.snapshot(), true
	}
	key = dialogKey(callID, tagB, tagA)
	if dlg, ok := s.dialogs[key]; ok {
		return dlg.snapshot(), true
	}
	return DialogState{}, false
}

// ExpireSessions removes dialogs that have exceeded their session interval.
func (s *Server) ExpireSessions(now time.Time) []DialogState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []DialogState
	for key, dlg := range s.dialogs {
		if dlg.SessionInterval <= 0 {
			continue
		}
		if now.After(dlg.LastUpdated.Add(dlg.SessionInterval)) {
			expired = append(expired, dlg.snapshot())
			delete(s.dialogs, key)
		}
	}
	return expired
}

func (d *dialog) snapshot() DialogState {
	return DialogState{
		CallID:          d.CallID,
		LocalTag:        d.LocalTag,
		RemoteTag:       d.RemoteTag,
		SessionInterval: d.SessionInterval,
		Refresher:       d.Refresher,
		LastUpdated:     d.LastUpdated,
	}
}

func dialogKey(callID string, tags ...string) string {
	cleaned := []string{}
	for _, tag := range tags {
		if tag != "" {
			cleaned = append(cleaned, tag)
		}
	}
	sort.Strings(cleaned)
	return fmt.Sprintf("%s|%s", callID, strings.Join(cleaned, "|"))
}

func (s *Server) generateTagLocked() string {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

func ensureTagPresent(headerValue, tag string) string {
	if tag == "" {
		return headerValue
	}
	if GetHeaderParam(headerValue, "tag") != "" {
		return headerValue
	}
	if strings.Contains(headerValue, ";") {
		return headerValue + fmt.Sprintf(";tag=%s", tag)
	}
	if strings.Contains(headerValue, ">") {
		parts := strings.Split(headerValue, ">")
		if len(parts) >= 2 {
			prefix := strings.Join(parts[:len(parts)-1], ">") + ">"
			suffix := parts[len(parts)-1]
			if strings.TrimSpace(suffix) == "" {
				return prefix + fmt.Sprintf(";tag=%s", tag)
			}
			return prefix + suffix + fmt.Sprintf(";tag=%s", tag)
		}
	}
	if headerValue == "" {
		return fmt.Sprintf(";tag=%s", tag)
	}
	return headerValue + fmt.Sprintf(";tag=%s", tag)
}

func parseSessionExpires(value string) (time.Duration, string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, "", nil
	}
	parts := strings.Split(value, ";")
	secondsStr := strings.TrimSpace(parts[0])
	seconds, err := strconv.Atoi(secondsStr)
	if err != nil || seconds < 0 {
		return 0, "", ErrInvalidMessage
	}
	var refresher string
	for _, part := range parts[1:] {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(kv[0]), "refresher") {
			refresher = strings.ToLower(strings.Trim(strings.TrimSpace(kv[1]), "\""))
		}
	}
	return time.Duration(seconds) * time.Second, refresher, nil
}

func parseMinSE(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func formatSessionExpires(interval time.Duration, refresher string) string {
	seconds := int(interval / time.Second)
	if refresher == "" {
		return fmt.Sprintf("%d", seconds)
	}
	return fmt.Sprintf("%d;refresher=%s", seconds, refresher)
}
