package sip

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestTransactionLayerCleansUpExpiredServerTransactions(t *testing.T) {
	toTransport := make(chan transportEvent, 1)
	toTU := make(chan tuEvent, 1)
	layer := newTransactionLayer(nil, toTransport, toTU, nil)
	layer.serverTxTTL = 10 * time.Millisecond

	req := newInvite()
	layer.handleRequest(context.Background(), transportEvent{Direction: directionDownstream, Message: req})

	if len(layer.serverTxns) != 1 {
		t.Fatalf("expected server transaction to be recorded, got %d", len(layer.serverTxns))
	}

	time.Sleep(15 * time.Millisecond)
	layer.cleanupTransactions(context.Background(), time.Now())

	if len(layer.serverTxns) != 0 {
		t.Fatalf("expected expired server transaction to be cleaned up, got %d remaining", len(layer.serverTxns))
	}
}

func TestTransactionLayerRetransmitsFinalResponses(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	layer := newTransactionLayer(nil, toTransport, make(chan tuEvent, 1), nil)
	layer.serverTxTTL = 10 * time.Millisecond
	layer.timerGInitial = time.Millisecond
	layer.timerGMax = 2 * time.Millisecond
	layer.timerHDuration = 6 * time.Millisecond

	req := newInvite()
	layer.handleRequest(ctx, transportEvent{Direction: directionDownstream, Message: req})

	resp := NewResponse(500, "Server Error")
	CopyHeaders(resp, req, "Via", "From", "To", "Call-ID", "CSeq")

	key := transactionKey(topViaBranch(req), strings.ToUpper(req.Method))
	layer.handleTUAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: key, Message: resp})

	time.Sleep(2 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case evt := <-toTransport:
		if evt.Message == nil || evt.Message.StatusCode != 500 {
			t.Fatalf("unexpected retransmitted response: %#v", evt.Message)
		}
	default:
		t.Fatalf("expected retransmitted final response")
	}

	time.Sleep(5 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if _, ok := layer.serverTxns[key]; ok {
		t.Fatalf("expected server transaction to be removed after timer H expiry")
	}
}

func TestInviteServerTransactionStopsRetransmissionsAfterAck(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	layer := newTransactionLayer(nil, toTransport, make(chan tuEvent, 1), nil)
	layer.serverTxTTL = 10 * time.Millisecond
	layer.timerGInitial = time.Millisecond
	layer.timerGMax = 2 * time.Millisecond
	layer.timerHDuration = 20 * time.Millisecond
	layer.timerIDuration = 3 * time.Millisecond

	req := newInvite()
	layer.handleRequest(ctx, transportEvent{Direction: directionDownstream, Message: req})

	resp := buildResponseFrom(req, 500, "Server Error")

	key := transactionKey(topViaBranch(req), strings.ToUpper(req.Method))
	layer.handleTUAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: key, Message: resp})

	select {
	case <-toTransport:
	default:
		t.Fatalf("expected immediate final response to be forwarded")
	}

	time.Sleep(2 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case <-toTransport:
	default:
		t.Fatalf("expected retransmission prior to ACK")
	}

	ack := req.Clone()
	ack.Method = "ACK"
	ack.SetHeader("CSeq", "314159 ACK")
	layer.handleRequest(ctx, transportEvent{Direction: directionDownstream, Message: ack})

	time.Sleep(3 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case evt := <-toTransport:
		t.Fatalf("unexpected retransmission after ACK: %#v", evt.Message)
	default:
	}

	time.Sleep(4 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if _, ok := layer.serverTxns[key]; ok {
		t.Fatalf("expected server transaction to be removed after timer I expiry")
	}
}

func TestNonInviteServerTransactionRetainedForTimerJ(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	layer := newTransactionLayer(nil, toTransport, make(chan tuEvent, 1), nil)
	layer.serverTxTTL = 20 * time.Millisecond
	layer.timerJDuration = 5 * time.Millisecond

	req := newOptions()
	layer.handleRequest(ctx, transportEvent{Direction: directionDownstream, Message: req})

	resp := buildResponseFrom(req, 200, "OK")

	key := transactionKey(topViaBranch(req), strings.ToUpper(req.Method))
	layer.handleTUAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: key, Message: resp})

	select {
	case <-toTransport:
	default:
		t.Fatalf("expected immediate final response to be forwarded")
	}

	time.Sleep(3 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if _, ok := layer.serverTxns[key]; !ok {
		t.Fatalf("expected server transaction to persist until timer J expires")
	}

	layer.handleRequest(ctx, transportEvent{Direction: directionDownstream, Message: req})

	select {
	case <-toTransport:
	default:
		t.Fatalf("expected cached response for retransmitted request")
	}

	time.Sleep(4 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if _, ok := layer.serverTxns[key]; ok {
		t.Fatalf("expected server transaction to be removed after timer J expiry")
	}
}

func TestInviteClientTransactionRetransmitsUntilProvisional(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	layer := newTransactionLayer(nil, toTransport, make(chan tuEvent, 1), nil)
	layer.timerAInitial = time.Millisecond
	layer.timerAMax = 2 * time.Millisecond
	layer.timerBDuration = 20 * time.Millisecond
	layer.timerCDuration = 50 * time.Millisecond

	invite := newInvite()
	branch := newBranchID()
	prependVia(invite, branch)
	action := tuAction{Kind: tuActionForwardRequest, ServerTxID: "down", ClientTxID: transactionKey(branch, "INVITE"), Message: invite}

	layer.handleTUAction(ctx, action)

	first, ok := <-toTransport
	if !ok || first.Message == nil || !first.Message.IsRequest() {
		t.Fatalf("expected forwarded invite, got %#v", first.Message)
	}

	time.Sleep(2 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	var retrans transportEvent
	select {
	case retrans = <-toTransport:
	default:
		t.Fatalf("expected retransmission prior to provisional response")
	}
	if retrans.Message == nil || !strings.EqualFold(retrans.Message.Method, "INVITE") {
		t.Fatalf("unexpected retransmission payload: %#v", retrans.Message)
	}

	ringing := buildResponseFrom(first.Message, 180, "Ringing")
	layer.handleResponse(ctx, transportEvent{Direction: directionUpstream, Message: ringing})

	time.Sleep(3 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case evt := <-toTransport:
		t.Fatalf("unexpected retransmission after provisional: %#v", evt.Message)
	default:
	}
}

func TestInviteClientTransactionTimerBGeneratesTimeout(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	toTU := make(chan tuEvent, 10)
	layer := newTransactionLayer(nil, toTransport, toTU, nil)
	layer.timerAInitial = time.Millisecond
	layer.timerAMax = 2 * time.Millisecond
	layer.timerBDuration = 6 * time.Millisecond
	layer.timerCDuration = 50 * time.Millisecond

	invite := newInvite()
	branch := newBranchID()
	prependVia(invite, branch)
	action := tuAction{Kind: tuActionForwardRequest, ServerTxID: "down", ClientTxID: transactionKey(branch, "INVITE"), Message: invite}

	layer.handleTUAction(ctx, action)
	<-toTransport

	time.Sleep(7 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case evt := <-toTU:
		if evt.Message == nil || evt.Message.StatusCode != 408 {
			t.Fatalf("expected 408 timeout response, got %#v", evt.Message)
		}
	default:
		t.Fatalf("expected TU timeout notification")
	}

	if _, ok := layer.clientTxns[transactionKey(branch, "INVITE")]; ok {
		t.Fatalf("expected invite client transaction to be removed after timer B")
	}
}

func TestInviteClientTransactionTimerDTerminatesAfterFinal(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	layer := newTransactionLayer(nil, toTransport, make(chan tuEvent, 1), nil)
	layer.timerAInitial = time.Millisecond
	layer.timerAMax = 2 * time.Millisecond
	layer.timerBDuration = 20 * time.Millisecond
	layer.timerDDuration = 5 * time.Millisecond

	invite := newInvite()
	branch := newBranchID()
	prependVia(invite, branch)
	action := tuAction{Kind: tuActionForwardRequest, ServerTxID: "down", ClientTxID: transactionKey(branch, "INVITE"), Message: invite}

	layer.handleTUAction(ctx, action)
	forwarded, _ := <-toTransport

	failure := buildResponseFrom(forwarded.Message, 500, "Server Error")
	layer.handleResponse(ctx, transportEvent{Direction: directionUpstream, Message: failure})

	if entry, ok := layer.clientTxns[transactionKey(branch, "INVITE")]; !ok || entry.terminateAt.IsZero() {
		t.Fatalf("expected client transaction to persist for timer D")
	}

	time.Sleep(6 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if _, ok := layer.clientTxns[transactionKey(branch, "INVITE")]; ok {
		t.Fatalf("expected invite client transaction to be removed after timer D")
	}
}

func TestInviteClientTransactionTimerCSendsCancel(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	toTU := make(chan tuEvent, 10)
	layer := newTransactionLayer(nil, toTransport, toTU, nil)
	layer.timerAInitial = 5 * time.Millisecond
	layer.timerAMax = 10 * time.Millisecond
	layer.timerBDuration = 100 * time.Millisecond
	layer.timerCDuration = 4 * time.Millisecond

	invite := newInvite()
	branch := newBranchID()
	prependVia(invite, branch)
	action := tuAction{Kind: tuActionForwardRequest, ServerTxID: "down", ClientTxID: transactionKey(branch, "INVITE"), Message: invite}

	layer.handleTUAction(ctx, action)
	<-toTransport

	time.Sleep(5 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	cancelSeen := false
	for {
		select {
		case evt := <-toTransport:
			if evt.Message != nil && strings.EqualFold(evt.Message.Method, "CANCEL") {
				cancelSeen = true
			}
		default:
			goto done
		}
	}
done:
	if !cancelSeen {
		t.Fatalf("expected CANCEL to be sent on timer C expiry")
	}

	select {
	case evt := <-toTU:
		if evt.Message == nil || evt.Message.StatusCode != 408 {
			t.Fatalf("expected 408 timeout after timer C, got %#v", evt.Message)
		}
	default:
		t.Fatalf("expected timeout response on timer C expiry")
	}
}

func TestNonInviteClientTransactionRetransmitsAndTerminates(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	toTU := make(chan tuEvent, 10)
	layer := newTransactionLayer(nil, toTransport, toTU, nil)
	layer.timerEInitial = time.Millisecond
	layer.timerEMax = 2 * time.Millisecond
	layer.timerFDuration = 7 * time.Millisecond
	layer.timerKDuration = 4 * time.Millisecond

	options := newOptions()
	branch := newBranchID()
	prependVia(options, branch)
	action := tuAction{Kind: tuActionForwardRequest, ServerTxID: "down", ClientTxID: transactionKey(branch, "OPTIONS"), Message: options}

	layer.handleTUAction(ctx, action)
	first, _ := <-toTransport

	time.Sleep(2 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case evt := <-toTransport:
		if evt.Message == nil || !strings.EqualFold(evt.Message.Method, "OPTIONS") {
			t.Fatalf("unexpected retransmission payload: %#v", evt.Message)
		}
	default:
		t.Fatalf("expected non-INVITE retransmission")
	}

	okResp := buildResponseFrom(first.Message, 200, "OK")
	layer.handleResponse(ctx, transportEvent{Direction: directionUpstream, Message: okResp})

	select {
	case evt := <-toTU:
		if evt.Message == nil || evt.Message.StatusCode != 200 {
			t.Fatalf("expected 200 forwarded to TU, got %#v", evt.Message)
		}
	default:
		t.Fatalf("expected final response to reach TU")
	}

	time.Sleep(2 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if entry, ok := layer.clientTxns[transactionKey(branch, "OPTIONS")]; !ok || entry.terminateAt.IsZero() {
		t.Fatalf("expected non-INVITE client transaction to wait for timer K")
	}

	time.Sleep(3 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	if _, ok := layer.clientTxns[transactionKey(branch, "OPTIONS")]; ok {
		t.Fatalf("expected non-INVITE client transaction to be removed after timer K")
	}

	if len(toTU) != 0 {
		t.Fatalf("did not expect TU notifications after final response")
	}
}

func TestNonInviteClientTransactionTimerFGeneratesTimeout(t *testing.T) {
	ctx := context.Background()
	toTransport := make(chan transportEvent, 10)
	toTU := make(chan tuEvent, 10)
	layer := newTransactionLayer(nil, toTransport, toTU, nil)
	layer.timerEInitial = time.Millisecond
	layer.timerEMax = 2 * time.Millisecond
	layer.timerFDuration = 6 * time.Millisecond

	options := newOptions()
	branch := newBranchID()
	prependVia(options, branch)
	action := tuAction{Kind: tuActionForwardRequest, ServerTxID: "down", ClientTxID: transactionKey(branch, "OPTIONS"), Message: options}

	layer.handleTUAction(ctx, action)
	<-toTransport

	time.Sleep(7 * time.Millisecond)
	layer.cleanupTransactions(ctx, time.Now())

	select {
	case evt := <-toTU:
		if evt.Message == nil || evt.Message.StatusCode != 408 {
			t.Fatalf("expected 408 timeout for non-INVITE, got %#v", evt.Message)
		}
	default:
		t.Fatalf("expected TU timeout notification for non-INVITE")
	}

	if _, ok := layer.clientTxns[transactionKey(branch, "OPTIONS")]; ok {
		t.Fatalf("expected non-INVITE client transaction to be removed after timer F")
	}
}
