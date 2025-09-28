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

type broadcastSession struct {
	original     *Message
	callKey      string
	cseqNumber   int
	forks        map[string]*broadcastFork
	forkOrder    []string
	winner       string
	finalised    bool
	canceled     bool
	bestStatus   int
	bestResponse *Message
	winningResp  *Message
}

type broadcastFork struct {
	clientTxID string
	branch     string
	requestURI string
	invite     *Message
	final      bool
	cancelled  bool
}

type transactionUser struct {
	events    <-chan tuEvent
	actions   chan<- tuAction
	registrar *Registrar
	broadcast *BroadcastPolicy
	sessions  map[string]*broadcastSession
	callIndex map[string]string
	wg        sync.WaitGroup
}

func newTransactionUser(events <-chan tuEvent, actions chan<- tuAction, registrar *Registrar, broadcast *BroadcastPolicy) *transactionUser {
	return &transactionUser{
		events:    events,
		actions:   actions,
		registrar: registrar,
		broadcast: broadcast,
		sessions:  make(map[string]*broadcastSession),
		callIndex: make(map[string]string),
	}
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
		if t.registrar != nil && strings.EqualFold(req.Method, "REGISTER") {
			if resp, handled := t.registrar.handleRegister(ctx, req); handled {
				if resp != nil {
					action := tuAction{
						Kind:       tuActionSendResponse,
						ServerTxID: event.ServerTxID,
						Message:    resp,
					}
					t.sendAction(ctx, action)
				}
				return
			}
		}
		if strings.EqualFold(req.Method, "CANCEL") {
			if t.handleBroadcastCancel(ctx, event, req) {
				return
			}
		}
		if strings.EqualFold(req.Method, "INVITE") {
			if t.handleBroadcastInvite(ctx, event, req) {
				return
			}
		}
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
		if t.handleBroadcastResponse(ctx, event, resp) {
			return
		}
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

func (t *transactionUser) handleBroadcastInvite(ctx context.Context, event tuEvent, req *Message) bool {
	if t.broadcast == nil {
		return false
	}
	targets := t.broadcast.Targets(req.RequestURI)
	if len(targets) == 0 {
		if t.broadcast.Has(req.RequestURI) {
			resp := NewResponse(404, "Not Found")
			CopyHeaders(resp, req, "Via", "From", "To", "Call-ID", "CSeq")
			if resp.GetHeader("To") == "" {
				resp.SetHeader("To", req.GetHeader("To"))
			}
			t.sendAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: event.ServerTxID, Message: resp})
			return true
		}
		return false
	}

	session := &broadcastSession{
		original:   req.Clone(),
		forks:      make(map[string]*broadcastFork, len(targets)),
		forkOrder:  make([]string, 0, len(targets)),
		bestStatus: -1,
	}
	callKey := callKeyFromMessage(req)
	if callKey != "" {
		t.callIndex[callKey] = event.ServerTxID
		session.callKey = callKey
	}
	if num, ok := parseCSeqNumber(req.GetHeader("CSeq")); ok {
		session.cseqNumber = num
	}
	t.sessions[event.ServerTxID] = session

	sent := 0
	for _, target := range targets {
		clone := req.Clone()
		clone.RequestURI = target
		branch := newBranchID()
		prependVia(clone, branch)
		decrementMaxForwards(clone)
		clientTxID := transactionKey(branch, strings.ToUpper(clone.Method))
		fork := &broadcastFork{
			clientTxID: clientTxID,
			branch:     branch,
			requestURI: target,
			invite:     clone.Clone(),
		}
		session.forks[clientTxID] = fork
		session.forkOrder = append(session.forkOrder, clientTxID)
		action := tuAction{
			Kind:       tuActionForwardRequest,
			ServerTxID: event.ServerTxID,
			ClientTxID: clientTxID,
			Message:    clone,
		}
		t.sendAction(ctx, action)
		sent++
	}

	if sent == 0 {
		delete(t.sessions, event.ServerTxID)
		if callKey != "" {
			delete(t.callIndex, callKey)
		}
		resp := NewResponse(404, "Not Found")
		CopyHeaders(resp, req, "Via", "From", "To", "Call-ID", "CSeq")
		if resp.GetHeader("To") == "" {
			resp.SetHeader("To", req.GetHeader("To"))
		}
		t.sendAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: event.ServerTxID, Message: resp})
	}
	return true
}

func (t *transactionUser) handleBroadcastCancel(ctx context.Context, event tuEvent, req *Message) bool {
	if len(t.sessions) == 0 {
		return false
	}
	callKey := callKeyFromMessage(req)
	if callKey == "" {
		return false
	}
	serverTxID, ok := t.callIndex[callKey]
	if !ok {
		return false
	}
	session, ok := t.sessions[serverTxID]
	if !ok {
		return false
	}

	resp := NewResponse(200, "OK")
	CopyHeaders(resp, req, "Via", "From", "To", "Call-ID", "CSeq")
	if resp.GetHeader("To") == "" {
		resp.SetHeader("To", req.GetHeader("To"))
	}
	t.sendAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: event.ServerTxID, Message: resp})

	session.canceled = true
	for _, fork := range session.forks {
		if fork == nil || fork.final {
			continue
		}
		t.sendCancelForFork(ctx, serverTxID, session, fork)
	}
	return true
}

func (t *transactionUser) handleBroadcastResponse(ctx context.Context, event tuEvent, resp *Message) bool {
	session, ok := t.sessions[event.ServerTxID]
	if !ok {
		return false
	}
	method := strings.ToUpper(cseqMethod(resp))
	if method == "CANCEL" {
		removeTopViaWithBranch(resp, keyBranch(event.ClientTxID))
		return true
	}

	fork, hasFork := session.forks[event.ClientTxID]
	if !hasFork {
		removeTopViaWithBranch(resp, keyBranch(event.ClientTxID))
		return true
	}
	removeTopViaWithBranch(resp, fork.branch)

	status := resp.StatusCode
	if status < 200 {
		if session.finalised {
			return true
		}
		t.sendAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: event.ServerTxID, ClientTxID: event.ClientTxID, Message: resp.Clone()})
		return true
	}

	fork.final = true

	if status < 300 {
		if session.winner == "" {
			session.winner = event.ClientTxID
			session.winningResp = resp.Clone()
			session.finalised = true
			t.sendAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: event.ServerTxID, ClientTxID: event.ClientTxID, Message: resp.Clone()})
			for id, other := range session.forks {
				if id == event.ClientTxID || other == nil || other.final {
					continue
				}
				t.sendCancelForFork(ctx, event.ServerTxID, session, other)
			}
		} else if event.ClientTxID != session.winner {
			t.sendByeForFork(ctx, event.ServerTxID, session, fork, resp)
		}
	} else {
		if session.bestResponse == nil || status > session.bestStatus {
			session.bestStatus = status
			session.bestResponse = resp.Clone()
		}
		if session.winner == "" && session.allForksFinal() {
			session.finalised = true
			best := session.bestResponse
			if best == nil {
				best = resp.Clone()
			}
			t.sendAction(ctx, tuAction{Kind: tuActionSendResponse, ServerTxID: event.ServerTxID, Message: best.Clone()})
		}
	}

	if session.finalised && session.allForksFinal() {
		t.cleanupBroadcastSession(event.ServerTxID, session)
	}
	return true
}

func (t *transactionUser) sendCancelForFork(ctx context.Context, serverTxID string, session *broadcastSession, fork *broadcastFork) {
	if fork == nil || fork.final || fork.cancelled {
		return
	}
	cancel := fork.invite.Clone()
	cancel.Method = "CANCEL"
	cancel.RequestURI = fork.requestURI
	cancel.Body = ""
	cancel.StatusCode = 0
	cancel.ReasonPhrase = ""
	cancel.SetHeader("CSeq", formatCSeq(session.cseqNumber, "CANCEL"))
	cancel.DelHeader("Content-Length")
	fork.cancelled = true
	action := tuAction{
		Kind:       tuActionForwardRequest,
		ServerTxID: serverTxID,
		ClientTxID: transactionKey(fork.branch, "CANCEL"),
		Message:    cancel,
	}
	t.sendAction(ctx, action)
}

func (t *transactionUser) sendByeForFork(ctx context.Context, serverTxID string, session *broadcastSession, fork *broadcastFork, resp *Message) {
	if fork == nil {
		return
	}
	bye := fork.invite.Clone()
	bye.Method = "BYE"
	bye.RequestURI = fork.requestURI
	bye.Body = ""
	bye.StatusCode = 0
	bye.ReasonPhrase = ""
	bye.SetHeader("CSeq", formatCSeq(session.cseqNumber+1, "BYE"))
	if contact := strings.TrimSpace(resp.GetHeader("Contact")); contact != "" {
		bye.RequestURI = contactAddress(contact)
		if bye.RequestURI == "" {
			bye.RequestURI = contact
		}
	}
	bye.DelHeader("Content-Length")
	branch := newBranchID()
	prependVia(bye, branch)
	decrementMaxForwards(bye)
	action := tuAction{
		Kind:       tuActionForwardRequest,
		ServerTxID: serverTxID,
		ClientTxID: transactionKey(branch, "BYE"),
		Message:    bye,
	}
	t.sendAction(ctx, action)
}

func (t *transactionUser) cleanupBroadcastSession(serverTxID string, session *broadcastSession) {
	delete(t.sessions, serverTxID)
	if session != nil && session.callKey != "" {
		delete(t.callIndex, session.callKey)
	}
}

func callKeyFromMessage(msg *Message) string {
	if msg == nil {
		return ""
	}
	callID := strings.TrimSpace(msg.GetHeader("Call-ID"))
	if callID == "" {
		return ""
	}
	cseq := strings.Fields(strings.TrimSpace(msg.GetHeader("CSeq")))
	if len(cseq) == 0 {
		return ""
	}
	return strings.ToLower(callID) + "|" + cseq[0]
}

func parseCSeqNumber(value string) (int, bool) {
	parts := strings.Fields(strings.TrimSpace(value))
	if len(parts) == 0 {
		return 0, false
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, false
	}
	return num, true
}

func formatCSeq(number int, method string) string {
	if number <= 0 {
		number = 1
	}
	return fmt.Sprintf("%d %s", number, strings.ToUpper(method))
}

func (s *broadcastSession) allForksFinal() bool {
	if s == nil {
		return true
	}
	for _, fork := range s.forks {
		if fork == nil {
			continue
		}
		if !fork.final {
			return false
		}
	}
	return true
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
