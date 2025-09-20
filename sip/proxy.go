package sip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type direction int

const (
	directionDownstream direction = iota
	directionUpstream
)

type transportEvent struct {
	Direction direction
	Message   *Message
}

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

// Proxy exposes a stateful SIP proxy composed of transport, transaction, and
// transaction-user layers connected through queues.
type Proxy struct {
	ctx    context.Context
	cancel context.CancelFunc

	clientIn  chan *Message
	serverIn  chan *Message
	clientOut chan *Message
	serverOut chan *Message

	transport    *transportLayer
	transactions *transactionLayer
	core         *transactionUser
}

// NewProxy constructs and starts a stateful SIP proxy.
func NewProxy() *Proxy {
	ctx, cancel := context.WithCancel(context.Background())

	clientIn := make(chan *Message, 32)
	serverIn := make(chan *Message, 32)
	clientOut := make(chan *Message, 32)
	serverOut := make(chan *Message, 32)

	transportToTxn := make(chan transportEvent, 32)
	txnToTransport := make(chan transportEvent, 32)
	txnToTU := make(chan tuEvent, 32)
	tuToTxn := make(chan tuAction, 32)

	proxy := &Proxy{
		ctx:       ctx,
		cancel:    cancel,
		clientIn:  clientIn,
		serverIn:  serverIn,
		clientOut: clientOut,
		serverOut: serverOut,
	}

	proxy.transport = newTransportLayer(clientIn, serverIn, clientOut, serverOut, transportToTxn, txnToTransport)
	proxy.transactions = newTransactionLayer(transportToTxn, txnToTransport, txnToTU, tuToTxn)
	proxy.core = newTransactionUser(txnToTU, tuToTxn)

	proxy.transport.start(ctx)
	proxy.transactions.start(ctx)
	proxy.core.start(ctx)

	return proxy
}

// SendFromClient enqueues a message as if it was received from a downstream
// client.
func (p *Proxy) SendFromClient(msg *Message) {
	if p == nil || msg == nil {
		return
	}
	clone := msg.Clone()
	select {
	case <-p.ctx.Done():
		return
	case p.clientIn <- clone:
	}
}

// SendFromServer enqueues a message as if it was received from an upstream
// server.
func (p *Proxy) SendFromServer(msg *Message) {
	if p == nil || msg == nil {
		return
	}
	clone := msg.Clone()
	select {
	case <-p.ctx.Done():
		return
	case p.serverIn <- clone:
	}
}

// NextToClient returns the next message ready to be sent toward the downstream
// client. The boolean return indicates whether a message was retrieved before
// the timeout elapsed.
func (p *Proxy) NextToClient(timeout time.Duration) (*Message, bool) {
	if p == nil {
		return nil, false
	}
	if timeout <= 0 {
		select {
		case msg, ok := <-p.clientOut:
			if !ok {
				return nil, false
			}
			return msg, true
		case <-p.ctx.Done():
			return nil, false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg, ok := <-p.clientOut:
		if !ok {
			return nil, false
		}
		return msg, true
	case <-timer.C:
		return nil, false
	case <-p.ctx.Done():
		return nil, false
	}
}

// NextToServer returns the next message ready to be sent toward the upstream
// server. The boolean return indicates whether a message was retrieved before
// the timeout elapsed.
func (p *Proxy) NextToServer(timeout time.Duration) (*Message, bool) {
	if p == nil {
		return nil, false
	}
	if timeout <= 0 {
		select {
		case msg, ok := <-p.serverOut:
			if !ok {
				return nil, false
			}
			return msg, true
		case <-p.ctx.Done():
			return nil, false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case msg, ok := <-p.serverOut:
		if !ok {
			return nil, false
		}
		return msg, true
	case <-timer.C:
		return nil, false
	case <-p.ctx.Done():
		return nil, false
	}
}

// Stop shuts down the proxy and waits for all layers to exit.
func (p *Proxy) Stop() {
	if p == nil {
		return
	}
	p.cancel()
	p.core.wait()
	p.transactions.wait()
	p.transport.wait()
}

type transportLayer struct {
	clientIn  chan *Message
	serverIn  chan *Message
	clientOut chan *Message
	serverOut chan *Message
	toTxn     chan<- transportEvent
	fromTxn   <-chan transportEvent
	wg        sync.WaitGroup
}

func newTransportLayer(clientIn, serverIn, clientOut, serverOut chan *Message, toTxn chan<- transportEvent, fromTxn <-chan transportEvent) *transportLayer {
	return &transportLayer{
		clientIn:  clientIn,
		serverIn:  serverIn,
		clientOut: clientOut,
		serverOut: serverOut,
		toTxn:     toTxn,
		fromTxn:   fromTxn,
	}
}

func (t *transportLayer) start(ctx context.Context) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(t.clientOut)
		defer close(t.serverOut)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-t.clientIn:
				if !ok {
					continue
				}
				if msg == nil {
					continue
				}
				clone := msg.Clone()
				clone.EnsureContentLength()
				select {
				case t.toTxn <- transportEvent{Direction: directionDownstream, Message: clone}:
				case <-ctx.Done():
					return
				}
			case msg, ok := <-t.serverIn:
				if !ok {
					continue
				}
				if msg == nil {
					continue
				}
				clone := msg.Clone()
				clone.EnsureContentLength()
				select {
				case t.toTxn <- transportEvent{Direction: directionUpstream, Message: clone}:
				case <-ctx.Done():
					return
				}
			case evt, ok := <-t.fromTxn:
				if !ok {
					return
				}
				if evt.Message == nil {
					continue
				}
				msg := evt.Message.Clone()
				msg.EnsureContentLength()
				switch evt.Direction {
				case directionDownstream:
					select {
					case t.clientOut <- msg:
					case <-ctx.Done():
						return
					}
				case directionUpstream:
					select {
					case t.serverOut <- msg:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
}

func (t *transportLayer) wait() {
	t.wg.Wait()
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

type transactionUser struct {
	events  <-chan tuEvent
	actions chan<- tuAction
	wg      sync.WaitGroup
}

func newTransactionUser(events <-chan tuEvent, actions chan<- tuAction) *transactionUser {
	return &transactionUser{events: events, actions: actions}
}

func (t *transactionUser) start(ctx context.Context) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(t.actions)
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-t.events:
				if !ok {
					return
				}
				t.handleEvent(ctx, event)
			}
		}
	}()
}

func (t *transactionUser) wait() {
	t.wg.Wait()
}

func (t *transactionUser) handleEvent(ctx context.Context, event tuEvent) {
	switch event.Kind {
	case tuEventRequest:
		if event.Message == nil {
			return
		}
		req := event.Message.Clone()
		branch := newBranchID()
		prependVia(req, branch)
		decrementMaxForwards(req)
		action := tuAction{
			Kind:       tuActionForwardRequest,
			ServerTxID: event.ServerTxID,
			ClientTxID: transactionKey(branch, strings.ToUpper(req.Method)),
			Message:    req,
		}
		t.sendAction(ctx, action)
	case tuEventResponse:
		if event.Message == nil {
			return
		}
		resp := event.Message.Clone()
		removeTopViaWithBranch(resp, keyBranch(event.ClientTxID))
		action := tuAction{
			Kind:       tuActionSendResponse,
			ServerTxID: event.ServerTxID,
			ClientTxID: event.ClientTxID,
			Message:    resp,
		}
		t.sendAction(ctx, action)
	}
}

func (t *transactionUser) sendAction(ctx context.Context, action tuAction) {
	if action.Message != nil {
		action.Message.EnsureContentLength()
	}
	select {
	case t.actions <- action:
	case <-ctx.Done():
	}
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

func prependVia(msg *Message, branch string) {
	if msg == nil {
		return
	}
	via := fmt.Sprintf("SIP/2.0/UDP proxy.local;branch=%s", branch)
	existing := msg.HeaderValues("Via")
	values := make([]string, 0, len(existing)+1)
	values = append(values, via)
	values = append(values, existing...)
	msg.SetHeader("Via", values...)
}

func removeTopViaWithBranch(msg *Message, branch string) {
	if msg == nil || branch == "" {
		return
	}
	values := msg.HeaderValues("Via")
	if len(values) == 0 {
		return
	}
	filtered := make([]string, 0, len(values))
	removed := false
	for _, value := range values {
		if !removed && strings.EqualFold(viaBranch(value), branch) {
			removed = true
			continue
		}
		filtered = append(filtered, value)
	}
	if len(filtered) == 0 {
		msg.DelHeader("Via")
		return
	}
	msg.SetHeader("Via", filtered...)
}

func decrementMaxForwards(msg *Message) {
	if msg == nil {
		return
	}
	raw := strings.TrimSpace(msg.GetHeader("Max-Forwards"))
	if raw == "" {
		return
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return
	}
	if value > 0 {
		value--
	}
	msg.SetHeader("Max-Forwards", strconv.Itoa(value))
}

func newBranchID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("z9hG4bK%x", time.Now().UnixNano())
	}
	return "z9hG4bK" + hex.EncodeToString(buf)
}
