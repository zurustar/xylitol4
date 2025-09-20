# Design Overview

This document describes the stateful SIP proxy implemented in the `sip`
package. The proxy follows RFC 3261 and focuses on providing a clean separation
between the transport, transaction, and transaction user (TU) layers. Each
layer owns its own goroutine and exchanges work exclusively through buffered
queues so that they remain independently testable.

## Source Layout

The recent refactor split the proxy into one file per architectural layer to
make the responsibilities easier to audit:

- `proxy.go` – public façade that wires channels, starts goroutines, and exposes
  the queue-based API to callers.
- `transport.go` – pure transport logic that clones messages, normalises
  `Content-Length`, and moves datagrams between the network-facing queues and
  the transaction layer.
- `transaction.go` – RFC 3261 server/client transaction state machines together
  with helper utilities for managing branches, CSeq parsing, and generating
  synthetic responses.
- `transaction_user.go` – stateless proxy behaviour that mutates SIP headers,
  allocates new branches, and chooses the correct direction when forwarding
  messages.

Each file only exports constructors and lifecycle helpers for its respective
layer, which keeps the layering boundaries explicit and mirrors the structure
outlined below.

## Layered Architecture

The proxy is exposed through the `Proxy` type. Constructing a proxy wires three
cooperating subsystems:

- **Transport layer** – Converts abstract "client" and "server" endpoints into
  `Message` events. It delivers inbound datagrams to the transaction layer and
  publishes outbound datagrams on per-direction queues. The transport layer has
  no SIP awareness beyond cloning messages and ensuring content length headers
  are present before sending.
- **Transaction layer** – Implements RFC 3261 server and client transactions for
  INVITE and non-INVITE requests. It owns the transaction state machines,
  handles retransmissions, and decides when responses should be cached or
  forwarded. Every interaction with neighbouring layers happens via typed
  events, keeping the state machines decoupled from transport or TU concerns.
- **Transaction user (proxy core)** – Represents the stateless proxy logic. It
  receives requests/responses from the transaction layer, performs proxy
  specific mutations (adding/removing Via headers, decrementing Max-Forwards),
  and feeds the adjusted messages back to the transaction layer.

The channel topology is `transport -> transaction -> TU -> transaction ->
transport`, forming two ring buffers (one for control and one for media) that
preserve ordering while preventing direct cross-layer calls.

## Transaction Management

The transaction layer maintains two maps: one for server transactions keyed by
branch parameters from downstream requests and another for client transactions
keyed by the branch values the proxy generates for forwarded requests. Each
transaction captures:

- The original request or most recent response for retransmission purposes.
- The RFC 3261 state (`Proceeding`, `Completed`, `Confirmed`, `Terminated` for
  INVITE server transactions; `Calling`, `Proceeding`, `Completed`,
  `Terminated` otherwise).
- Directional information so that responses can be emitted toward the correct
  transport queue.

Server transactions emit a TU event the first time they observe a new request
branch. Subsequent retransmissions are intercepted and satisfied using the last
stored response without re-invoking upper layers. Client transactions tie an
upstream branch to the originating server transaction so that responses from
far-end servers can be routed back to the waiting downstream transaction.

## Proxy Core Behaviour

The TU layer acts as a simple, always-forwarding proxy:

1. **Requests** – When a request event arrives, the TU clones the message,
   decrements `Max-Forwards` when present, prepends a new Via header containing a
   freshly generated branch (prefixed with `z9hG4bK`), and instructs the
   transaction layer to create a client transaction that forwards the request
   upstream.
2. **Responses** – Responses from upstream arrive with the proxy's Via header on
   top. The TU removes that hop, leaving the next Via ready for the downstream
   client, and tells the transaction layer to relay the response via the matched
   server transaction.

This small amount of SIP intelligence is confined to the TU, leaving both the
transport and transaction layers unaware of proxy-specific policy.

## Public Surface

Tests interact with the proxy via four queues exposed on `Proxy`:

- `SendFromClient` / `SendFromServer` enqueue messages as if they were received
  from downstream clients or upstream servers.
- `NextToClient` / `NextToServer` read the datagrams ready to be sent in either
  direction.
- `Stop` shuts down the proxy by cancelling the shared context and waiting for
  all layer goroutines to exit.

All APIs clone messages before handing them to other layers to avoid accidental
sharing. Responses are rendered with up-to-date `Content-Length` headers just
before they reach the transport layer.

## Error Handling

Malformed requests that lack a branch parameter or otherwise violate expectations
are answered immediately with a 400 response generated inside the transaction
layer. Unexpected responses are dropped. These choices keep the state machines
robust while remaining faithful to the behaviour required by RFC 3261 for a
stateful proxy.

## Command Entrypoint

The `cmd/sip-proxy` package wires the proxy to real UDP sockets so it can run as a
standalone executable. A small supervisor in `main.go` is responsible for:

- binding one socket for downstream clients and one for the upstream server;
- decoding incoming datagrams with `sip.ParseMessage` and feeding them into the
  proxy;
- remembering the origin address for each downstream transaction via a TTL based
  `transactionRouter` so that responses emerging from `Proxy.NextToClient` can be
  written back to the right client; and
- terminating goroutines cleanly when the process receives `SIGINT` or `SIGTERM`.

The router extends the TTL on every successful lookup and a background cleanup
loop prunes expired entries, ensuring memory usage remains bounded even for busy
systems.
