package sip

import (
	"context"
	"sync"
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
