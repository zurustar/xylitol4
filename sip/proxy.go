package sip

import (
	"context"
	"time"
)

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
