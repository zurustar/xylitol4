package sip

import (
	"strings"
	"testing"
	"time"
)

func TestProxyInviteTransactionFlow(t *testing.T) {
	proxy := NewProxy()
	t.Cleanup(proxy.Stop)

	invite := newInvite()
	proxy.SendFromClient(invite)

	forwarded, ok := proxy.NextToServer(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected forwarded invite")
	}
	if forwarded.Method != "INVITE" {
		t.Fatalf("unexpected method: %s", forwarded.Method)
	}

	forwardedVias := forwarded.HeaderValues("Via")
	if len(forwardedVias) < 2 {
		t.Fatalf("expected proxy to prepend Via header: %v", forwardedVias)
	}
	insertedBranch := viaBranch(forwardedVias[0])
	if !strings.HasPrefix(insertedBranch, "z9hG4bK") {
		t.Fatalf("proxy branch should start with z9hG4bK: %s", insertedBranch)
	}
	originalBranch := viaBranch(forwardedVias[1])
	if originalBranch != viaBranch(invite.GetHeader("Via")) {
		t.Fatalf("original branch lost: got %s", originalBranch)
	}
	if mf := forwarded.GetHeader("Max-Forwards"); mf != "69" {
		t.Fatalf("expected max-forwards decrement, got %s", mf)
	}

	ringing := buildResponseFrom(forwarded, 180, "Ringing")
	proxy.SendFromServer(ringing)

	downstream, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected ringing response downstream")
	}
	if downstream.StatusCode != 180 {
		t.Fatalf("unexpected ringing status: %d", downstream.StatusCode)
	}
	viaValues := downstream.HeaderValues("Via")
	if len(viaValues) != len(forwardedVias)-1 {
		t.Fatalf("expected proxy Via stripped, got %v", viaValues)
	}
	if viaBranch(viaValues[0]) != originalBranch {
		t.Fatalf("unexpected top via branch after stripping: %s", viaBranch(viaValues[0]))
	}

	okResp := buildResponseFrom(forwarded, 200, "OK")
	proxy.SendFromServer(okResp)

	final, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected final response downstream")
	}
	if final.StatusCode != 200 {
		t.Fatalf("unexpected final status: %d", final.StatusCode)
	}

	proxy.SendFromClient(invite)

	retrans, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected retransmission response")
	}
	if retrans.StatusCode != 200 {
		t.Fatalf("expected cached final response, got %d", retrans.StatusCode)
	}
	if _, ok := proxy.NextToServer(50 * time.Millisecond); ok {
		t.Fatalf("retransmission should not be forwarded upstream")
	}
}

func TestProxyNonInviteTransactionRetransmission(t *testing.T) {
	proxy := NewProxy()
	t.Cleanup(proxy.Stop)

	options := newOptions()
	proxy.SendFromClient(options)

	forwarded, ok := proxy.NextToServer(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected forwarded options request")
	}
	if forwarded.Method != "OPTIONS" {
		t.Fatalf("unexpected method: %s", forwarded.Method)
	}
	forwardedVias := forwarded.HeaderValues("Via")
	if len(forwardedVias) < 2 {
		t.Fatalf("expected Via stack to include proxy entry: %v", forwardedVias)
	}
	insertedBranch := viaBranch(forwardedVias[0])
	if insertedBranch == viaBranch(forwardedVias[1]) {
		t.Fatalf("proxy should generate new branch")
	}

	okResp := buildResponseFrom(forwarded, 200, "OK")
	proxy.SendFromServer(okResp)

	downstream, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected downstream response")
	}
	if downstream.StatusCode != 200 {
		t.Fatalf("unexpected status: %d", downstream.StatusCode)
	}

	proxy.SendFromClient(options)

	retrans, ok := proxy.NextToClient(100 * time.Millisecond)
	if !ok {
		t.Fatalf("expected cached response for retransmission")
	}
	if retrans.StatusCode != 200 {
		t.Fatalf("expected cached 200 OK, got %d", retrans.StatusCode)
	}
	if _, ok := proxy.NextToServer(50 * time.Millisecond); ok {
		t.Fatalf("retransmitted request should not traverse upstream")
	}
}

func buildResponseFrom(req *Message, status int, reason string) *Message {
	resp := NewResponse(status, reason)
	if req != nil {
		vias := req.HeaderValues("Via")
		if len(vias) > 0 {
			resp.SetHeader("Via", vias[0])
			for _, via := range vias[1:] {
				resp.AddHeader("Via", via)
			}
		}
		CopyHeaders(resp, req, "From", "To", "Call-ID", "CSeq")
	}
	resp.SetHeader("Content-Length", "0")
	return resp
}

func newInvite() *Message {
	msg := NewRequest("INVITE", "sip:bob@example.com")
	msg.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient1")
	msg.SetHeader("From", "\"Alice\" <sip:alice@example.com>;tag=1928301774")
	msg.SetHeader("To", "<sip:bob@example.com>")
	msg.SetHeader("Call-ID", "a84b4c76e66710")
	msg.SetHeader("CSeq", "314159 INVITE")
	msg.SetHeader("Max-Forwards", "70")
	msg.SetHeader("Contact", "<sip:alice@client.example.com>")
	msg.SetHeader("Content-Length", "0")
	return msg
}

func newOptions() *Message {
	msg := NewRequest("OPTIONS", "sip:bob@example.com")
	msg.SetHeader("Via", "SIP/2.0/UDP client.example.com;branch=z9hG4bKclient2")
	msg.SetHeader("From", "\"Alice\" <sip:alice@example.com>;tag=1928301774")
	msg.SetHeader("To", "<sip:bob@example.com>")
	msg.SetHeader("Call-ID", "b84b4c76e66711")
	msg.SetHeader("CSeq", "314159 OPTIONS")
	msg.SetHeader("Max-Forwards", "70")
	msg.SetHeader("Content-Length", "0")
	return msg
}
