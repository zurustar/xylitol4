# Design Overview

This document captures the architecture and major components of the SIP server
implementation contained in this repository. The goal of the package is to
provide a small stateful SIP user agent server (UAS) that can negotiate and
maintain session timers while exposing enough structure for embedding into
larger systems.

## Message Model

The `sip` package defines a single `Message` type that models both SIP requests
and responses. Important characteristics include:

- **Core fields** – The struct tracks whether the instance is a request via the
  unexported `isRequest` flag and stores the method, request URI, protocol
  version, status code, reason phrase, header map, and body payload.
- **Construction helpers** – `NewRequest` and `NewResponse` initialise requests
  and responses respectively. They normalise method casing, default the
  protocol to `SIP/2.0`, and populate a header map.
- **Cloning and inspection** – `Clone` performs a deep copy of the message and
  `IsRequest` exposes whether an instance originated as a request.
- **Header management** – Convenience helpers (`SetHeader`, `AddHeader`,
  `DelHeader`, `GetHeader`, `HeaderValues`) manage canonicalised headers. The
  implementation uses `textproto.CanonicalMIMEHeaderKey` to normalise header
  names and always copies slices to avoid accidental sharing.
- **Content-Length** – `EnsureContentLength` synchronises the `Content-Length`
  header with the body prior to serialisation.
- **Encoding** – `String` renders the message to on-the-wire text by emitting
  the start line, sorted headers, and body. It delegates to
  `EnsureContentLength` to guarantee a correct payload size.

### Parsing and Helpers

`ParseMessage` converts raw wire data to a `Message` by reading the start line
and MIME-style headers via `textproto.Reader`. For responses the start line is
interpreted as `SIP/<version> <status> <reason>`; for requests it parses
`<method> <uri> <version>`. A body is read according to `Content-Length` when
present, otherwise any remaining bytes are treated as the payload. Validation
errors surface as `ErrInvalidMessage`.

Additional helpers simplify SIP header manipulation:

- `CopyHeaders` copies selected headers between messages, cloning slices.
- `GetHeaderParam`, `replaceHeaderParam`, and `ensureHeaderParam` operate on
  semicolon-delimited header parameters.
- `ensureHeaderValue` guarantees a specific token exists in a comma-delimited
  header, avoiding duplicates.
- `FormatHeader` renders `"Name: value"` pairs using canonical casing.

These utilities underpin the server's response construction and session timer
handling.

## Server Architecture

The `Server` type orchestrates SIP dialog management and request handling. Its
notable fields are:

- `dialogs` – A map from a stable dialog key to internal `dialog` records.
- `defaultSessionInterval` – Baseline session expiration when negotiation does
  not supply an explicit interval.
- `contact` – Value used for the `Contact` header on responses.
- `now` – Clock function injected via options for testing.
- `mu` – A mutex protecting access to shared dialog state.

Construction uses functional options (`Option`). `WithDefaultSessionInterval`,
`WithContact`, and `WithClock` customise behaviour while `NewServer` applies
sane fallbacks.

Dialog state is represented internally by the `dialog` struct (Call-ID, local
and remote tags, session interval, refresher role, and last-update timestamp).
A public `DialogState` mirrors this data for read-only inspection. Methods such
as `ActiveDialogs`, `DialogState`, and `dialog.snapshot()` expose consistent
snapshots while holding the mutex.

## Request Handling Flow

`Handle` accepts raw UDP payload text, parses it with `ParseMessage`, and then
delegates to `HandleMessage`. The latter acts on already parsed requests and
selects specialised handlers based on the SIP method:

- `INVITE` → `handleInvite`
- `BYE` → `handleBye`
- `UPDATE` → `handleUpdate`
- `OPTIONS` → immediate 200 OK containing capability headers
- `ACK` → no response (ACK is hop-by-hop and terminates the transaction)
- Any other method → 501 Not Implemented

Each handler constructs responses via `buildResponse`, which copies mandatory
headers (`Via`, `From`, `Call-ID`, `CSeq`) and preserves an existing `To`
header. It also populates `Content-Length` when the body is empty.

### INVITE Lifecycle

`handleInvite` drives dialog creation and negotiation:

1. Validates the presence of `Call-ID` and `From` tag parameters.
2. Reuses an existing dialog when one is already tracked for the Call-ID and
   tags; otherwise it creates a new entry, generating a local `To` tag when the
   request omits one.
3. Parses `Session-Expires` through `parseSessionExpires` and honours `Min-SE`
   if no explicit interval exists. Defaults fall back to the server's configured
   session interval.
4. Updates the dialog's session interval, refresher role, and timestamp.
5. Returns a 200 OK containing capability headers (`Allow`, `Supported`,
   `Require`) and the negotiated `Session-Expires`.

### UPDATE Lifecycle

`handleUpdate` looks up the dialog (allowing for swapped tags when the server is
not the refresher), validates the request, and merges new session timer values
from `Session-Expires`. It responds with a 200 OK mirroring the INVITE
capability headers while leaving dialog state consistent.

### BYE Lifecycle

`handleBye` removes dialogs. It attempts both tag orderings to support BYE
requests initiated by either endpoint. If no dialog is found it returns 481
(Call/Transaction Does Not Exist); otherwise it acknowledges with a 200 OK and
cleans up the map entry.

## Session Timer Management

Session timers rely on helper functions:

- `parseSessionExpires` parses the interval in seconds and optional
  `refresher` parameter from the `Session-Expires` header.
- `parseMinSE` reads the `Min-SE` header to enforce minimum timers.
- `formatSessionExpires` renders negotiated values for responses.
- `ensureTagPresent` injects a local `tag` parameter into the `To` header when
  creating or updating dialogs.

`ExpireSessions` allows external callers to purge dialogs whose `LastUpdated`
plus `SessionInterval` is older than the supplied reference time. The method
returns the removed `DialogState` records for further processing or logging.

`dialogKey` builds a stable identifier by combining the Call-ID with sorted tag
values so that lookups succeed regardless of caller ordering.

## Transport Layer

`ServeUDP` is a convenience loop that binds a UDP socket (defaulting to port
5060), reads inbound datagrams, processes them through `Handle`, and writes the
resulting wire-encoded responses back to the sender. Errors in request parsing
silently drop the offending packet while the server continues servicing
subsequent requests.

This layering keeps networking concerns optional: embedders can provide their
own transport by calling `HandleMessage` directly with parsed `Message` values.

