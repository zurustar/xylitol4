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

type transactionUser struct {
	events    <-chan tuEvent
	actions   chan<- tuAction
	registrar *Registrar
	wg        sync.WaitGroup
}

func newTransactionUser(events <-chan tuEvent, actions chan<- tuAction, registrar *Registrar) *transactionUser {
	return &transactionUser{events: events, actions: actions, registrar: registrar}
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
