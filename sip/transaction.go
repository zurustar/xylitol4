package sip

import (
	"context"
	"strings"
	"sync"
)

type tuEventKind int

const (
	tuEventRequest tuEventKind = iota
	tuEventResponse
)

type tuEvent struct {
	Kind       tuEventKind
	ServerTxID string
	ClientTxID string
	Message    *Message
}

type tuActionKind int

const (
	tuActionForwardRequest tuActionKind = iota
	tuActionSendResponse
)

type tuAction struct {
	Kind       tuActionKind
	ServerTxID string
	ClientTxID string
	Message    *Message
}

type serverTransactionState int

type serverTransaction struct {
	id           string
	branch       string
	method       string
	state        serverTransactionState
	request      *Message
	lastResponse *Message
}

type clientTransactionState int

type clientTransaction struct {
	id           string
	branch       string
	method       string
	state        clientTransactionState
	request      *Message
	lastResponse *Message
	serverTxID   string
}

type transactionLayer struct {
	fromTransport <-chan transportEvent
	toTransport   chan<- transportEvent
	toTU          chan<- tuEvent
	fromTU        <-chan tuAction

	serverTxns map[string]*serverTransaction
	clientTxns map[string]*clientTransaction

	wg sync.WaitGroup
}

func newTransactionLayer(fromTransport <-chan transportEvent, toTransport chan<- transportEvent, toTU chan<- tuEvent, fromTU <-chan tuAction) *transactionLayer {
	return &transactionLayer{
		fromTransport: fromTransport,
		toTransport:   toTransport,
		toTU:          toTU,
		fromTU:        fromTU,
		serverTxns:    make(map[string]*serverTransaction),
		clientTxns:    make(map[string]*clientTransaction),
	}
}

func (t *transactionLayer) start(ctx context.Context) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(t.toTransport)
		defer close(t.toTU)
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-t.fromTransport:
				if !ok {
					return
				}
				if evt.Message == nil {
					continue
				}
				t.handleTransportEvent(ctx, evt)
			case action, ok := <-t.fromTU:
				if !ok {
					return
				}
				t.handleTUAction(ctx, action)
			}
		}
	}()
}

func (t *transactionLayer) wait() {
	t.wg.Wait()
}

func (t *transactionLayer) handleTransportEvent(ctx context.Context, evt transportEvent) {
	if evt.Message == nil {
		return
	}
	if evt.Message.IsRequest() {
		t.handleRequest(ctx, evt)
		return
	}
	t.handleResponse(ctx, evt)
}

func (t *transactionLayer) handleRequest(ctx context.Context, evt transportEvent) {
	req := evt.Message
	branch := topViaBranch(req)
	if branch == "" {
		t.rejectRequest(ctx, req, 400, "Missing branch")
		return
	}
	method := strings.ToUpper(req.Method)
	key := transactionKey(branch, method)
	if existing, ok := t.serverTxns[key]; ok {
		if existing.lastResponse != nil {
			resp := existing.lastResponse.Clone()
			t.sendToTransport(ctx, transportEvent{Direction: directionDownstream, Message: resp})
		}
		return
	}
	txn := &serverTransaction{
		id:      key,
		branch:  branch,
		method:  method,
		state:   0,
		request: req.Clone(),
	}
	t.serverTxns[key] = txn
	event := tuEvent{
		Kind:       tuEventRequest,
		ServerTxID: key,
		Message:    req.Clone(),
	}
	t.sendToTU(ctx, event)
}

func (t *transactionLayer) handleResponse(ctx context.Context, evt transportEvent) {
	resp := evt.Message
	branch := topViaBranch(resp)
	if branch == "" {
		return
	}
	method := cseqMethod(resp)
	if method == "" {
		return
	}
	key := transactionKey(branch, method)
	txn, ok := t.clientTxns[key]
	if !ok {
		return
	}
	txn.lastResponse = resp.Clone()
	status := resp.StatusCode
	if status < 200 {
		txn.state = 1
	} else {
		txn.state = 2
		delete(t.clientTxns, key)
	}
	event := tuEvent{
		Kind:       tuEventResponse,
		ServerTxID: txn.serverTxID,
		ClientTxID: key,
		Message:    resp.Clone(),
	}
	t.sendToTU(ctx, event)
}

func (t *transactionLayer) handleTUAction(ctx context.Context, action tuAction) {
	switch action.Kind {
	case tuActionForwardRequest:
		if action.Message == nil {
			return
		}
		branch := topViaBranch(action.Message)
		if branch == "" {
			branch = keyBranch(action.ClientTxID)
			if branch == "" {
				branch = newBranchID()
			}
		}
		method := strings.ToUpper(action.Message.Method)
		key := action.ClientTxID
		if key == "" {
			key = transactionKey(branch, method)
		}
		txn := &clientTransaction{
			id:         key,
			branch:     branch,
			method:     method,
			state:      0,
			request:    action.Message.Clone(),
			serverTxID: action.ServerTxID,
		}
		t.clientTxns[key] = txn
		t.sendToTransport(ctx, transportEvent{Direction: directionUpstream, Message: action.Message.Clone()})
	case tuActionSendResponse:
		if action.Message == nil {
			return
		}
		txn, ok := t.serverTxns[action.ServerTxID]
		if !ok {
			return
		}
		resp := action.Message.Clone()
		txn.lastResponse = resp.Clone()
		status := resp.StatusCode
		if status < 200 {
			txn.state = 0
		} else {
			if txn.method == "INVITE" {
				if status >= 200 && status < 300 {
					txn.state = 3
				} else {
					txn.state = 2
				}
			} else {
				txn.state = 3
			}
		}
		t.sendToTransport(ctx, transportEvent{Direction: directionDownstream, Message: resp})
	}
}

func (t *transactionLayer) sendToTransport(ctx context.Context, evt transportEvent) {
	if evt.Message != nil {
		evt.Message.EnsureContentLength()
	}
	select {
	case t.toTransport <- evt:
	case <-ctx.Done():
	}
}

func (t *transactionLayer) sendToTU(ctx context.Context, event tuEvent) {
	if event.Message != nil {
		event.Message.EnsureContentLength()
	}
	select {
	case t.toTU <- event:
	case <-ctx.Done():
	}
}

func (t *transactionLayer) rejectRequest(ctx context.Context, req *Message, status int, reason string) {
	resp := NewResponse(status, reason)
	if req != nil {
		CopyHeaders(resp, req, "Via", "From", "To", "Call-ID", "CSeq")
		if resp.GetHeader("To") == "" {
			resp.SetHeader("To", req.GetHeader("To"))
		}
	}
	resp.EnsureContentLength()
	t.sendToTransport(ctx, transportEvent{Direction: directionDownstream, Message: resp})
}

func topViaBranch(msg *Message) string {
	if msg == nil {
		return ""
	}
	values := msg.HeaderValues("Via")
	if len(values) == 0 {
		return ""
	}
	return viaBranch(values[0])
}

func viaBranch(value string) string {
	segments := strings.Split(value, ";")
	for _, segment := range segments[1:] {
		kv := strings.SplitN(strings.TrimSpace(segment), "=", 2)
		if len(kv) != 2 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(kv[0]), "branch") {
			return strings.Trim(strings.TrimSpace(kv[1]), "\"")
		}
	}
	return ""
}

func cseqMethod(msg *Message) string {
	if msg == nil {
		return ""
	}
	cseq := strings.TrimSpace(msg.GetHeader("CSeq"))
	if cseq == "" {
		return ""
	}
	parts := strings.Fields(cseq)
	if len(parts) < 2 {
		return ""
	}
	return strings.ToUpper(parts[1])
}

func transactionKey(branch, method string) string {
	return strings.ToUpper(method) + "|" + branch
}

func keyBranch(key string) string {
	parts := strings.SplitN(key, "|", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}
