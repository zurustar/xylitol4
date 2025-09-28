package sip

import (
	"context"
	"strings"
	"sync"
	"time"
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

type transactionData struct {
	id           string
	branch       string
	method       string
	request      *Message
	lastResponse *Message
}

type serverTransaction interface {
	data() *transactionData
	onSendResponse(status int)
}

type clientTransaction interface {
	data() *transactionData
	onReceiveResponse(status int) bool
	onTimeout()
	serverID() string
}

func newServerTransactionForMethod(method string, data *transactionData) serverTransaction {
	if strings.EqualFold(method, "INVITE") {
		return newInviteServerTransaction(data)
	}
	return newNonInviteServerTransaction(data)
}

func newClientTransactionForMethod(method string, data *transactionData, serverTxID string) clientTransaction {
	if strings.EqualFold(method, "INVITE") {
		return newInviteClientTransaction(data, serverTxID)
	}
	return newNonInviteClientTransaction(data, serverTxID)
}

type transactionLayer struct {
	fromTransport <-chan transportEvent
	toTransport   chan<- transportEvent
	toTU          chan<- tuEvent
	fromTU        <-chan tuAction

	serverTxns map[string]serverTransactionEntry
	clientTxns map[string]clientTransactionEntry

	serverTxTTL     time.Duration
	cleanupInterval time.Duration
	timerGInitial   time.Duration
	timerGMax       time.Duration
	timerHDuration  time.Duration
	timerIDuration  time.Duration
	timerJDuration  time.Duration
	timerAInitial   time.Duration
	timerAMax       time.Duration
	timerBDuration  time.Duration
	timerCDuration  time.Duration
	timerDDuration  time.Duration
	timerEInitial   time.Duration
	timerEMax       time.Duration
	timerFDuration  time.Duration
	timerKDuration  time.Duration

	wg sync.WaitGroup
}

type serverTransactionEntry struct {
	txn      serverTransaction
	expires  time.Time
	deadline time.Time

	retransmitAt       time.Time
	retransmitInterval time.Duration
}

type clientTransactionEntry struct {
	txn                clientTransaction
	deadline           time.Time
	retransmitAt       time.Time
	retransmitInterval time.Duration
	terminateAt        time.Time
	timerCDeadline     time.Time
}

const (
	defaultServerTransactionTTL      = 32 * time.Second
	serverTransactionCleanupInterval = time.Second
	defaultTimerT1                   = 500 * time.Millisecond
	defaultTimerT2                   = 4 * time.Second
	defaultTimerT4                   = 5 * time.Second
	defaultTimerGInitial             = defaultTimerT1
	defaultTimerGMax                 = defaultTimerT2
	defaultTimerH                    = 64 * defaultTimerT1
	defaultTimerI                    = defaultTimerT4
	defaultTimerJ                    = 64 * defaultTimerT1
	defaultTimerAInitial             = defaultTimerT1
	defaultTimerAMax                 = defaultTimerT2
	defaultTimerB                    = 64 * defaultTimerT1
	defaultTimerC                    = 3 * time.Minute
	defaultTimerD                    = 32 * time.Second
	defaultTimerEInitial             = defaultTimerT1
	defaultTimerEMax                 = defaultTimerT2
	defaultTimerF                    = 64 * defaultTimerT1
	defaultTimerK                    = defaultTimerT4
)

func newTransactionLayer(fromTransport <-chan transportEvent, toTransport chan<- transportEvent, toTU chan<- tuEvent, fromTU <-chan tuAction) *transactionLayer {
	return &transactionLayer{
		fromTransport:   fromTransport,
		toTransport:     toTransport,
		toTU:            toTU,
		fromTU:          fromTU,
		serverTxns:      make(map[string]serverTransactionEntry),
		clientTxns:      make(map[string]clientTransactionEntry),
		serverTxTTL:     defaultServerTransactionTTL,
		cleanupInterval: serverTransactionCleanupInterval,
		timerGInitial:   defaultTimerGInitial,
		timerGMax:       defaultTimerGMax,
		timerHDuration:  defaultTimerH,
		timerIDuration:  defaultTimerI,
		timerJDuration:  defaultTimerJ,
		timerAInitial:   defaultTimerAInitial,
		timerAMax:       defaultTimerAMax,
		timerBDuration:  defaultTimerB,
		timerCDuration:  defaultTimerC,
		timerDDuration:  defaultTimerD,
		timerEInitial:   defaultTimerEInitial,
		timerEMax:       defaultTimerEMax,
		timerFDuration:  defaultTimerF,
		timerKDuration:  defaultTimerK,
	}
}

func (t *transactionLayer) start(ctx context.Context) {
	interval := t.cleanupInterval
	if interval <= 0 {
		interval = serverTransactionCleanupInterval
	}
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer close(t.toTransport)
		defer close(t.toTU)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				t.cleanupTransactions(ctx, now)
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
	if method == "ACK" {
		t.handleAck(branch)
		return
	}
	key := transactionKey(branch, method)
	if entry, ok := t.serverTxns[key]; ok {
		if data := entry.txn.data(); data != nil && data.lastResponse != nil {
			resp := data.lastResponse.Clone()
			t.sendToTransport(ctx, transportEvent{Direction: directionDownstream, Message: resp})
		}
		entry.expires = time.Now().Add(t.serverTransactionRetention())
		t.serverTxns[key] = entry
		return
	}
	txnData := &transactionData{
		id:      key,
		branch:  branch,
		method:  method,
		request: req.Clone(),
	}
	txn := newServerTransactionForMethod(method, txnData)
	now := time.Now()
	t.serverTxns[key] = serverTransactionEntry{
		txn:     txn,
		expires: now.Add(t.serverTransactionRetention()),
	}
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
	entry, ok := t.clientTxns[key]
	if !ok {
		return
	}
	txn := entry.txn
	if data := txn.data(); data != nil {
		data.lastResponse = resp.Clone()
	}
	status := resp.StatusCode
	completed := txn.onReceiveResponse(status)
	now := time.Now()
	switch txn.(type) {
	case *inviteClientTransaction:
		entry.deadline = time.Time{}
		entry.retransmitAt = time.Time{}
		entry.retransmitInterval = 0
		if status < 200 {
			if entry.timerCDeadline.IsZero() {
				if timeout := t.timerC(); timeout > 0 {
					entry.timerCDeadline = now.Add(timeout)
				}
			}
			t.clientTxns[key] = entry
			break
		}
		entry.timerCDeadline = time.Time{}
		if status < 300 {
			delete(t.clientTxns, key)
			break
		}
		if timeout := t.timerD(); timeout > 0 {
			entry.terminateAt = now.Add(timeout)
			t.clientTxns[key] = entry
		} else {
			txn.onTimeout()
			delete(t.clientTxns, key)
		}
	default:
		if status < 200 {
			interval := t.timerEMaxInterval()
			if interval <= 0 {
				entry.retransmitAt = time.Time{}
				entry.retransmitInterval = 0
			} else {
				entry.retransmitInterval = interval
				entry.retransmitAt = now.Add(interval)
			}
			t.clientTxns[key] = entry
			break
		}
		entry.deadline = time.Time{}
		entry.retransmitAt = time.Time{}
		entry.retransmitInterval = 0
		if timeout := t.timerK(); timeout > 0 {
			entry.terminateAt = now.Add(timeout)
			t.clientTxns[key] = entry
		} else {
			txn.onTimeout()
			delete(t.clientTxns, key)
		}
	}
	if completed {
		delete(t.clientTxns, key)
	}
	event := tuEvent{
		Kind:       tuEventResponse,
		ServerTxID: txn.serverID(),
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
		txnData := &transactionData{
			id:      key,
			branch:  branch,
			method:  method,
			request: action.Message.Clone(),
		}
		txn := newClientTransactionForMethod(method, txnData, action.ServerTxID)
		entry := clientTransactionEntry{txn: txn}
		now := time.Now()
		switch txn.(type) {
		case *inviteClientTransaction:
			if interval := t.timerAStart(); interval > 0 {
				entry.retransmitInterval = interval
				entry.retransmitAt = now.Add(interval)
			}
			if timeout := t.timerB(); timeout > 0 {
				entry.deadline = now.Add(timeout)
			}
			if timeout := t.timerC(); timeout > 0 {
				entry.timerCDeadline = now.Add(timeout)
			}
		default:
			if interval := t.timerEStart(); interval > 0 {
				entry.retransmitInterval = interval
				entry.retransmitAt = now.Add(interval)
			}
			if timeout := t.timerF(); timeout > 0 {
				entry.deadline = now.Add(timeout)
			}
		}
		t.clientTxns[key] = entry
		t.sendToTransport(ctx, transportEvent{Direction: directionUpstream, Message: action.Message.Clone()})
	case tuActionSendResponse:
		if action.Message == nil {
			return
		}
		entry, ok := t.serverTxns[action.ServerTxID]
		if !ok {
			return
		}
		resp := action.Message.Clone()
		if data := entry.txn.data(); data != nil {
			data.lastResponse = resp.Clone()
		}
		status := resp.StatusCode
		entry.txn.onSendResponse(status)
		now := time.Now()
		entry.expires = now.Add(t.serverTransactionRetention())
		if status >= 200 {
			switch entry.txn.(type) {
			case *inviteServerTransaction:
				if status >= 300 {
					entry.deadline = now.Add(t.timerH())
					entry.retransmitInterval = t.timerGStart()
					entry.retransmitAt = now.Add(entry.retransmitInterval)
				} else {
					entry.deadline = now.Add(t.timerH())
					entry.retransmitInterval = 0
					entry.retransmitAt = time.Time{}
				}
			default:
				entry.deadline = now.Add(t.timerJ())
				entry.retransmitInterval = 0
				entry.retransmitAt = time.Time{}
			}
		}
		t.serverTxns[action.ServerTxID] = entry
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

func (t *transactionLayer) serverTransactionRetention() time.Duration {
	if t == nil || t.serverTxTTL <= 0 {
		return defaultServerTransactionTTL
	}
	return t.serverTxTTL
}

func (t *transactionLayer) timerGStart() time.Duration {
	if t == nil || t.timerGInitial <= 0 {
		return defaultTimerGInitial
	}
	return t.timerGInitial
}

func (t *transactionLayer) timerGMaxInterval() time.Duration {
	if t == nil || t.timerGMax <= 0 {
		return defaultTimerGMax
	}
	return t.timerGMax
}

func (t *transactionLayer) timerH() time.Duration {
	if t == nil || t.timerHDuration <= 0 {
		return defaultTimerH
	}
	return t.timerHDuration
}

func (t *transactionLayer) timerI() time.Duration {
	if t == nil || t.timerIDuration <= 0 {
		return defaultTimerI
	}
	return t.timerIDuration
}

func (t *transactionLayer) timerJ() time.Duration {
	if t == nil || t.timerJDuration <= 0 {
		return defaultTimerJ
	}
	return t.timerJDuration
}

func (t *transactionLayer) timerAStart() time.Duration {
	if t == nil || t.timerAInitial <= 0 {
		return defaultTimerAInitial
	}
	return t.timerAInitial
}

func (t *transactionLayer) timerAMaxInterval() time.Duration {
	if t == nil || t.timerAMax <= 0 {
		return defaultTimerAMax
	}
	return t.timerAMax
}

func (t *transactionLayer) timerB() time.Duration {
	if t == nil || t.timerBDuration <= 0 {
		return defaultTimerB
	}
	return t.timerBDuration
}

func (t *transactionLayer) timerC() time.Duration {
	if t == nil || t.timerCDuration <= 0 {
		return defaultTimerC
	}
	return t.timerCDuration
}

func (t *transactionLayer) timerD() time.Duration {
	if t == nil || t.timerDDuration <= 0 {
		return defaultTimerD
	}
	return t.timerDDuration
}

func (t *transactionLayer) timerEStart() time.Duration {
	if t == nil || t.timerEInitial <= 0 {
		return defaultTimerEInitial
	}
	return t.timerEInitial
}

func (t *transactionLayer) timerEMaxInterval() time.Duration {
	if t == nil || t.timerEMax <= 0 {
		return defaultTimerEMax
	}
	return t.timerEMax
}

func (t *transactionLayer) timerF() time.Duration {
	if t == nil || t.timerFDuration <= 0 {
		return defaultTimerF
	}
	return t.timerFDuration
}

func (t *transactionLayer) timerK() time.Duration {
	if t == nil || t.timerKDuration <= 0 {
		return defaultTimerK
	}
	return t.timerKDuration
}

func (t *transactionLayer) cleanupTransactions(ctx context.Context, now time.Time) {
	if len(t.serverTxns) > 0 {
		retention := t.serverTransactionRetention()
		maxInterval := t.timerGMaxInterval()
		for key, entry := range t.serverTxns {
			if !entry.deadline.IsZero() && now.After(entry.deadline) {
				delete(t.serverTxns, key)
				continue
			}

			if !entry.retransmitAt.IsZero() && (now.Equal(entry.retransmitAt) || now.After(entry.retransmitAt)) {
				if data := entry.txn.data(); data != nil && data.lastResponse != nil {
					t.sendToTransport(ctx, transportEvent{Direction: directionDownstream, Message: data.lastResponse.Clone()})
					if entry.retransmitInterval <= 0 {
						entry.retransmitInterval = t.timerGStart()
					} else {
						entry.retransmitInterval *= 2
						if maxInterval > 0 && entry.retransmitInterval > maxInterval {
							entry.retransmitInterval = maxInterval
						}
					}
					entry.retransmitAt = now.Add(entry.retransmitInterval)
					entry.expires = now.Add(retention)
					t.serverTxns[key] = entry
				} else {
					entry.retransmitAt = time.Time{}
					entry.retransmitInterval = 0
					t.serverTxns[key] = entry
				}
				continue
			}

			if now.After(entry.expires) {
				delete(t.serverTxns, key)
			}
		}
	}

	if len(t.clientTxns) == 0 {
		return
	}
	inviteMax := t.timerAMaxInterval()
	nonInviteMax := t.timerEMaxInterval()
	for key, entry := range t.clientTxns {
		txn := entry.txn
		data := txn.data()

		if !entry.deadline.IsZero() && (now.Equal(entry.deadline) || now.After(entry.deadline)) {
			if resp := timeoutResponseFromRequest(data, 408, "Request Timeout"); resp != nil {
				txn.onTimeout()
				t.sendToTU(ctx, tuEvent{Kind: tuEventResponse, ServerTxID: txn.serverID(), ClientTxID: key, Message: resp})
			}
			delete(t.clientTxns, key)
			continue
		}

		if !entry.timerCDeadline.IsZero() && (now.Equal(entry.timerCDeadline) || now.After(entry.timerCDeadline)) {
			if cancel := cancelFromRequest(data); cancel != nil {
				t.sendToTransport(ctx, transportEvent{Direction: directionUpstream, Message: cancel})
			}
			if resp := timeoutResponseFromRequest(data, 408, "Request Timeout"); resp != nil {
				txn.onTimeout()
				t.sendToTU(ctx, tuEvent{Kind: tuEventResponse, ServerTxID: txn.serverID(), ClientTxID: key, Message: resp})
			}
			delete(t.clientTxns, key)
			continue
		}

		if !entry.retransmitAt.IsZero() && (now.Equal(entry.retransmitAt) || now.After(entry.retransmitAt)) {
			if data != nil && data.request != nil {
				t.sendToTransport(ctx, transportEvent{Direction: directionUpstream, Message: data.request.Clone()})
				switch txn.(type) {
				case *inviteClientTransaction:
					if entry.retransmitInterval <= 0 {
						entry.retransmitInterval = t.timerAStart()
					} else {
						entry.retransmitInterval *= 2
						if inviteMax > 0 && entry.retransmitInterval > inviteMax {
							entry.retransmitInterval = inviteMax
						}
					}
				default:
					if entry.retransmitInterval <= 0 {
						entry.retransmitInterval = t.timerEStart()
					} else {
						entry.retransmitInterval *= 2
						if nonInviteMax > 0 && entry.retransmitInterval > nonInviteMax {
							entry.retransmitInterval = nonInviteMax
						}
					}
				}
				entry.retransmitAt = now.Add(entry.retransmitInterval)
				t.clientTxns[key] = entry
			} else {
				entry.retransmitAt = time.Time{}
				entry.retransmitInterval = 0
				t.clientTxns[key] = entry
			}
			continue
		}

		if !entry.terminateAt.IsZero() && (now.Equal(entry.terminateAt) || now.After(entry.terminateAt)) {
			txn.onTimeout()
			delete(t.clientTxns, key)
		}
	}
}

func (t *transactionLayer) handleAck(branch string) {
	if branch == "" {
		return
	}
	key := transactionKey(branch, "INVITE")
	entry, ok := t.serverTxns[key]
	if !ok {
		return
	}
	invite, ok := entry.txn.(*inviteServerTransaction)
	if !ok {
		return
	}
	if !invite.onReceiveAck() {
		return
	}
	timeout := t.timerI()
	if timeout <= 0 {
		delete(t.serverTxns, key)
		return
	}
	now := time.Now()
	entry.deadline = now.Add(timeout)
	entry.retransmitInterval = 0
	entry.retransmitAt = time.Time{}
	entry.expires = now.Add(t.serverTransactionRetention())
	t.serverTxns[key] = entry
}

func timeoutResponseFromRequest(data *transactionData, status int, reason string) *Message {
	if data == nil || data.request == nil {
		return nil
	}
	resp := NewResponse(status, reason)
	CopyHeaders(resp, data.request, "Via", "From", "To", "Call-ID", "CSeq")
	if resp.GetHeader("To") == "" {
		resp.SetHeader("To", data.request.GetHeader("To"))
	}
	resp.EnsureContentLength()
	return resp
}

func cancelFromRequest(data *transactionData) *Message {
	if data == nil || data.request == nil {
		return nil
	}
	req := data.request
	cancel := req.Clone()
	cancel.isRequest = true
	cancel.Method = "CANCEL"
	cancel.StatusCode = 0
	cancel.ReasonPhrase = ""
	cancel.Body = ""
	if number, ok := parseCSeqNumber(req.GetHeader("CSeq")); ok {
		cancel.SetHeader("CSeq", formatCSeq(number, "CANCEL"))
	} else {
		cancel.SetHeader("CSeq", formatCSeq(1, "CANCEL"))
	}
	cancel.EnsureContentLength()
	return cancel
}
