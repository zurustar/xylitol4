package sip

import (
	"strings"
	"testing"
	"time"
)

func TestParseAndRenderMessage(t *testing.T) {
	raw := "INVITE sip:bob@example.com SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP client.example.com;branch=z9hG4bKnashds8\r\n" +
		"From: \"Alice\" <sip:alice@example.com>;tag=1928301774\r\n" +
		"To: <sip:bob@example.com>\r\n" +
		"Call-ID: a84b4c76e66710\r\n" +
		"CSeq: 314159 INVITE\r\n" +
		"Max-Forwards: 70\r\n" +
		"Contact: <sip:alice@client.example.com>\r\n" +
		"Content-Length: 0\r\n\r\n"

	msg, err := ParseMessage(raw)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !msg.IsRequest() {
		t.Fatalf("expected request")
	}
	if msg.Method != "INVITE" {
		t.Fatalf("unexpected method: %s", msg.Method)
	}
	if got := msg.GetHeader("From"); !strings.Contains(got, "Alice") {
		t.Fatalf("unexpected From header: %s", got)
	}

	encoded := msg.String()
	if !strings.Contains(encoded, "INVITE sip:bob@example.com SIP/2.0") {
		t.Fatalf("missing request line in encoded message: %s", encoded)
	}
	if !strings.Contains(encoded, "Content-Length: 0") {
		t.Fatalf("content-length not set: %s", encoded)
	}
}

func TestServerHandleInviteCreatesDialog(t *testing.T) {
	current := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	srv := NewServer(
		WithClock(func() time.Time { return current }),
		WithDefaultSessionInterval(180*time.Second),
		WithContact("<sip:proxy@example.com>"),
	)

	req := newInviteRequest("90;refresher=uac")
	responses, err := srv.HandleMessage(req)
	if err != nil {
		t.Fatalf("handle invite failed: %v", err)
	}
	if len(responses) != 1 {
		t.Fatalf("expected one response, got %d", len(responses))
	}

	resp := responses[0]
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	if got := resp.GetHeader("Session-Expires"); got != "90;refresher=uac" {
		t.Fatalf("unexpected Session-Expires header: %s", got)
	}
	if got := resp.GetHeader("Supported"); got != "timer" {
		t.Fatalf("timer support missing: %s", got)
	}
	if got := resp.GetHeader("Require"); got != "timer" {
		t.Fatalf("timer requirement missing: %s", got)
	}
	if got := resp.GetHeader("Contact"); got != "<sip:proxy@example.com>" {
		t.Fatalf("unexpected contact header: %s", got)
	}

	toTag := GetHeaderParam(resp.GetHeader("To"), "tag")
	if toTag == "" {
		t.Fatalf("expected dialog tag in response")
	}

	state, ok := srv.DialogState("a84b4c76e66710", "1928301774", toTag)
	if !ok {
		t.Fatalf("dialog not stored")
	}
	if state.SessionInterval != 90*time.Second {
		t.Fatalf("unexpected session interval: %v", state.SessionInterval)
	}
	if state.Refresher != "uac" {
		t.Fatalf("unexpected refresher: %s", state.Refresher)
	}
	if state.LastUpdated != current {
		t.Fatalf("unexpected last update time: %v", state.LastUpdated)
	}
}

func TestServerHandlesReinviteAndUpdate(t *testing.T) {
	current := time.Date(2024, 2, 3, 4, 5, 6, 0, time.UTC)
	srv := NewServer(
		WithClock(func() time.Time { return current }),
		WithDefaultSessionInterval(120*time.Second),
	)

	invite := newInviteRequest("90;refresher=uac")
	responses, err := srv.HandleMessage(invite)
	if err != nil {
		t.Fatalf("initial invite failed: %v", err)
	}
	toTag := GetHeaderParam(responses[0].GetHeader("To"), "tag")
	if toTag == "" {
		t.Fatalf("missing to tag")
	}

	current = current.Add(30 * time.Second)

	reinvite := newInviteRequest("180;refresher=uas")
	reinvite.SetHeader("To", ensureTagPresent(reinvite.GetHeader("To"), toTag))
	reinvite.SetHeader("CSeq", "314160 INVITE")
	srv.now = func() time.Time { return current }

	responses, err = srv.HandleMessage(reinvite)
	if err != nil {
		t.Fatalf("re-invite failed: %v", err)
	}
	resp := responses[0]
	if got := resp.GetHeader("Session-Expires"); got != "180;refresher=uas" {
		t.Fatalf("unexpected response session interval: %s", got)
	}

	state, ok := srv.DialogState("a84b4c76e66710", "1928301774", toTag)
	if !ok {
		t.Fatalf("dialog missing after re-invite")
	}
	if state.SessionInterval != 180*time.Second {
		t.Fatalf("unexpected session interval: %v", state.SessionInterval)
	}
	if state.Refresher != "uas" {
		t.Fatalf("unexpected refresher after re-invite: %s", state.Refresher)
	}
	if state.LastUpdated != current {
		t.Fatalf("expected last updated to be re-invite time")
	}

	current = current.Add(40 * time.Second)
	update := newUpdateRequest(toTag, "200;refresher=uac")
	srv.now = func() time.Time { return current }
	responses, err = srv.HandleMessage(update)
	if err != nil {
		t.Fatalf("update failed: %v", err)
	}
	resp = responses[0]
	if got := resp.GetHeader("Session-Expires"); got != "200;refresher=uac" {
		t.Fatalf("unexpected session-expires on update: %s", got)
	}
	state, ok = srv.DialogState("a84b4c76e66710", "1928301774", toTag)
	if !ok {
		t.Fatalf("dialog missing after update")
	}
	if state.SessionInterval != 200*time.Second {
		t.Fatalf("unexpected interval after update: %v", state.SessionInterval)
	}
	if state.Refresher != "uac" {
		t.Fatalf("unexpected refresher after update: %s", state.Refresher)
	}
	if state.LastUpdated != current {
		t.Fatalf("expected last updated after update")
	}
}

func TestServerHandleByeRemovesDialog(t *testing.T) {
	current := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
	srv := NewServer(
		WithClock(func() time.Time { return current }),
		WithDefaultSessionInterval(90*time.Second),
	)

	invite := newInviteRequest("90")
	responses, err := srv.HandleMessage(invite)
	if err != nil {
		t.Fatalf("invite failed: %v", err)
	}
	toTag := GetHeaderParam(responses[0].GetHeader("To"), "tag")

	bye := newByeRequest(toTag)
	srv.now = func() time.Time { return current.Add(10 * time.Second) }
	responses, err = srv.HandleMessage(bye)
	if err != nil {
		t.Fatalf("bye failed: %v", err)
	}
	if len(responses) != 1 || responses[0].StatusCode != 200 {
		t.Fatalf("unexpected bye response: %+v", responses)
	}
	if _, ok := srv.DialogState("a84b4c76e66710", "1928301774", toTag); ok {
		t.Fatalf("dialog should be removed after BYE")
	}
}

func TestExpireSessions(t *testing.T) {
	current := time.Date(2024, 4, 5, 6, 7, 8, 0, time.UTC)
	srv := NewServer(
		WithClock(func() time.Time { return current }),
		WithDefaultSessionInterval(60*time.Second),
	)

	invite := newInviteRequest("60")
	responses, err := srv.HandleMessage(invite)
	if err != nil {
		t.Fatalf("invite failed: %v", err)
	}
	toTag := GetHeaderParam(responses[0].GetHeader("To"), "tag")
	if toTag == "" {
		t.Fatalf("missing tag")
	}

	expired := srv.ExpireSessions(current.Add(30 * time.Second))
	if len(expired) != 0 {
		t.Fatalf("unexpected expirations: %+v", expired)
	}

	expired = srv.ExpireSessions(current.Add(2 * time.Minute))
	if len(expired) != 1 {
		t.Fatalf("expected one expired dialog, got %d", len(expired))
	}
	if expired[0].CallID != "a84b4c76e66710" {
		t.Fatalf("unexpected expired call-id: %s", expired[0].CallID)
	}
	if _, ok := srv.DialogState("a84b4c76e66710", "1928301774", toTag); ok {
		t.Fatalf("dialog should be gone after expiration")
	}
}

func newInviteRequest(sessionExpires string) *Message {
	msg := NewRequest("INVITE", "sip:bob@example.com")
	msg.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKnashds8")
	msg.SetHeader("From", "\"Alice\" <sip:alice@example.com>;tag=1928301774")
	msg.SetHeader("To", "<sip:bob@example.com>")
	msg.SetHeader("Call-ID", "a84b4c76e66710")
	msg.SetHeader("CSeq", "314159 INVITE")
	msg.SetHeader("Max-Forwards", "70")
	msg.SetHeader("Contact", "<sip:alice@client.example.com>")
	if sessionExpires != "" {
		msg.SetHeader("Session-Expires", sessionExpires)
	}
	msg.SetHeader("Content-Length", "0")
	return msg
}

func newUpdateRequest(toTag, sessionExpires string) *Message {
	msg := NewRequest("UPDATE", "sip:bob@example.com")
	msg.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKnashds8")
	msg.SetHeader("From", "\"Alice\" <sip:alice@example.com>;tag=1928301774")
	msg.SetHeader("To", ensureTagPresent("<sip:bob@example.com>", toTag))
	msg.SetHeader("Call-ID", "a84b4c76e66710")
	msg.SetHeader("CSeq", "314161 UPDATE")
	msg.SetHeader("Max-Forwards", "70")
	msg.SetHeader("Contact", "<sip:alice@client.example.com>")
	if sessionExpires != "" {
		msg.SetHeader("Session-Expires", sessionExpires)
	}
	msg.SetHeader("Content-Length", "0")
	return msg
}

func newByeRequest(toTag string) *Message {
	msg := NewRequest("BYE", "sip:bob@example.com")
	msg.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKnashds8")
	msg.SetHeader("From", ensureTagPresent("\"Alice\" <sip:alice@example.com>", "1928301774"))
	msg.SetHeader("To", ensureTagPresent("<sip:bob@example.com>", toTag))
	msg.SetHeader("Call-ID", "a84b4c76e66710")
	msg.SetHeader("CSeq", "314162 BYE")
	msg.SetHeader("Max-Forwards", "70")
	msg.SetHeader("Content-Length", "0")
	return msg
}
