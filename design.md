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
- `stack.go` – process integration layer that loads the user directory, opens
  network sockets, and supervises the long-running goroutines behind the
  `SIPStack` type used by the executable.
- `transport.go` – pure transport logic that clones messages, normalises
  `Content-Length`, and moves datagrams between the network-facing queues and
  the transaction layer.
- `transaction.go` – transaction orchestrator that owns the registries,
  dispatches transport/TU events, and instantiates typed transactions while
  keeping the shared `transactionData` container.
- `invite_server_transaction.go` / `non_invite_server_transaction.go` – INVITE
  and non-INVITE server transaction state machines backed by `iota`-declared
  enums that mirror RFC 3261 terminal states.
- `invite_client_transaction.go` / `non_invite_client_transaction.go` –
  corresponding client transaction state machines with their own enumerated
  lifecycles and links to the originating server transaction.
- `transaction_user.go` – stateless proxy behaviour that mutates SIP headers,
  allocates new branches, and chooses the correct direction when forwarding
  messages.
- `message.go` – core SIP message representation, parser, and renderer shared by
  every layer and the command-line entrypoint.
- `registrar.go` – built-in registrar that authenticates REGISTER requests and
  stores contact bindings for downstream lookups.
- `userdb/` – SQLite-backed user directory helpers plus the in-memory driver
  used by tests to exercise the same queries without CGO.

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
entry wraps a shared `transactionData` struct (containing the ID, branch,
method, original request, and cached response) together with a state machine
specific to INVITE or non-INVITE processing. The state machines live in their
own files and use `iota` based enums to make transitions explicit while keeping
the reusable data container free of SIP-specific behaviour.

Server transactions emit a TU event the first time they observe a new request
branch. Subsequent retransmissions are intercepted and satisfied using the last
stored response without re-invoking upper layers. Client transactions keep the
same shared data and additionally record the originating server transaction ID;
this `serverTxID` is included with TU notifications so that responses received
from far-end servers can be routed back to the waiting downstream transaction,
even when multiple forks are active for the same dialog-less request.

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

## Upstream Routing

To operate as an upstream server the stack now derives the next hop for every
forwarded request dynamically. At startup `SIPStack` records the set of domains
present in the user directory together with any statically configured contact
URIs. When a request is ready to be forwarded, the stack resolves the target in
the following order:

1. **Registrar bindings** – If the Request-URI domain is managed locally and the
   registrar has an active binding for the target user, the associated contact
   URI is parsed and used as the transport destination.
2. **Directory defaults** – When no live registration exists, any `ContactURI`
   stored in the directory entry serves as the fallback address for that user.
3. **Request-URI host** – Otherwise the host/port portion of the Request-URI is
   resolved directly (including literal IP addresses).
4. **Configured default upstream** – If all of the above fail and a default
   `--upstream` address was supplied, the message is sent there for final
   resolution.

This strategy allows the binary to run without a mandatory upstream hop while
remaining compatible with the previous configuration style that specified a
single forwarding address.

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

## User Directory and Registrar Data

Registrar-facing logic requires access to user credentials and registered
contact URIs. To keep this data source encapsulated, the `sip/userdb` package
wraps a SQLite database behind a small `SQLiteStore` that exposes read-only
helpers (`Lookup` and `AllUsers`). The store constrains the driver to a single
connection so that it remains safe for concurrent use by the proxy while still
surfacing a standard `database/sql` handle for schema initialisation in tests.

Unit tests avoid CGO by relying on a pure Go, in-memory SQLite driver
implemented in `sqlite_driver.go`. The driver registers itself as
`sql.Register("sqlite", ...)`, supports `CREATE TABLE`, `INSERT`, and `SELECT`
statements, and applies very small SQL parsing helpers tailored to the schema
used by the proxy. This keeps the test suite hermetic while exercising the same
query paths the production proxy uses.

The command-line entrypoint now requires a `--user-db` flag that points to the
SQLite datasource. On startup the proxy opens the store, eagerly loads all
directory entries for logging/validation, and keeps the handle available for the
transaction user. This guarantees that future registrar features can rely on the
database being available before any network traffic is processed.

### Broadcast Ringing Configuration

To support simultaneous ringing, the user directory now tracks **broadcast
rules** in addition to standard user records. Each rule captures the address of
record that should trigger a broadcast (for example, `sip:sales@example.com`) and
stores the ordered list of downstream contact URIs that must be called in
parallel. The new `BroadcastRule`/`BroadcastTarget` types in `sip/userdb` expose
CRUD helpers so administrative tooling can create, update, and delete these
mappings. Consumers load the rules at runtime through `ListBroadcastRules` or
`LookupBroadcastTargets`, ensuring the proxy can discover all destinations that
must be forked when an INVITE arrives for a broadcast-enabled address.

When the SIP stack boots it now pulls every broadcast rule from SQLite, converts
them into the in-memory `BroadcastPolicy`, and wires the policy into the proxy
via `sip.WithBroadcastPolicy`. The transaction user consults this policy whenever
an INVITE arrives: if the Request-URI matches a broadcast address, it clones the
request for each contact, assigns a unique branch identifier, and forwards every
fork upstream in parallel while tracking the per-branch state inside a
`broadcastSession`. The session records provisional responses, forwards the first
2xx back downstream, and immediately emits CANCEL requests for the losing forks.
Late 2xx answers trigger a best-effort BYE so the remote leg tears down cleanly,
and failure responses are aggregated so the caller eventually receives the most
informative final status when no branch succeeds. CANCEL requests coming from the
downstream caller are also fanned out to every active fork, and the proxy caches
the best failure response until all branches complete before replying with 487.

The management portal (`cmd/user-web`) gained new panels for broadcast ringing.
Administrators can list existing rules, create new address-to-target mappings,
replace a rule's contact list in bulk, or delete unused entries. Targets are
entered as newline or comma separated SIP URIs and are persisted in priority
order so the runtime policy preserves the configured ringing sequence.

## Registrar Behaviour

The proxy embeds an optional registrar that can be supplied at construction time
(`sip.WithRegistrar`). When configured, the transaction user intercepts
`REGISTER` requests and handles them locally instead of forwarding them
upstream. The registrar authenticates clients using HTTP Digest credentials
fetched from the user database, challenges unauthenticated requests with a 401
`WWW-Authenticate` header, and replies with 403 for invalid credentials.

Successful registrations update an in-memory contact binding table keyed by the
Address of Record. Each binding tracks the contact URI and its expiry, honouring
per-contact `expires` parameters or the global `Expires` header with a sensible
default. Responses include the active bindings along with a freshly minted `To`
tag so retransmissions can be matched correctly. Wildcard contacts with
`Expires: 0` clear all bindings for the user.

The registrar exposes the stored bindings through `BindingsFor`, which the unit
tests use to verify state transitions. The command-line proxy automatically
constructs a registrar backed by the SQLite user store, ensuring REGISTER
traffic is validated and recorded without involving the upstream server.

## Command Entrypoint

The `cmd/sip-proxy` package wires the proxy to real UDP sockets so it can run as a
standalone executable. Runtime behaviour is configured through command-line flags:
`--listen` selects the downstream bind address, `--upstream` chooses the target
server, `--upstream-bind` pins the local address for upstream traffic, and
`--route-ttl` controls how long transaction routes are cached. A `--user-db`
argument is also required so the process can open the SQLite-backed directory,
eagerly load all entries for logging, and construct the registrar used for
REGISTER handling. These responsibilities now live inside the `SIPStack` type in
`sip/stack.go`, which opens the sockets, loads the user directory, instantiates the
registrar-backed proxy, and supervises the worker goroutines that bridge the network
with the queue-based proxy API. The stack also owns the TTL-based `transactionRouter`
used to remember downstream routes and runs the periodic cleanup loop that prunes
expired entries. Additional flags (`--http-listen`, `--admin-user`, and
`--admin-pass`) enable the web UI to be served from the same binary; when supplied,
the command opens a second SQLite handle dedicated to HTTP traffic and wires the
templates exposed by `internal/userweb` into an `http.Server`.

`main.go` continues to own flag parsing and signal handling but now orchestrates two
long-running services. It constructs a `SIPStack`, calls `Start` with the
signal-aware context, and then, if administrative credentials were provided, starts
the HTTP server in its own goroutine while monitoring an error channel. Shutdown is
coordinated across both subsystems by cancelling the shared context, invoking
`http.Server.Shutdown` with a timeout, and finally calling `SIPStack.Stop` so the
proxy and the web UI exit cleanly together.

## Web管理インタフェース

SQLiteベースのユーザディレクトリを直接操作できるWeb UIは`internal/userweb`パッケージにまとまり、`cmd/sip-proxy`から同一プロセスで利用される。HTTP Basic認証で保護された`/admin/users`エンドポイントではユーザ一覧の表示、初期パスワードやContact URIを指定したユーザ登録、既存ユーザの削除をフォームで提供する。これらの操作は`sip/userdb.SQLiteStore`に追加した`CreateUser`/`DeleteUser`/`UpdatePassword`メソッド経由で実行される。利用者向けの`/password`エンドポイントでは現在のパスワードを検証したうえで`HashPassword`/`VerifyPassword`ヘルパーを用いて新しいパスワードをHA1ダイジェストとして保存する。テンプレートは`html/template`で組み込み、一覧はドメイン・ユーザ名順にソートして表示する。SIPスタックとは別のSQLite接続を開いた上でHTTPサーバを起動し、プロセスの終了時に`http.Server.Shutdown`で安全に停止させることで、SIP処理とWeb UIを一括で管理できるようになった。
