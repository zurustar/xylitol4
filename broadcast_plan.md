# Broadcast Ringing Feature Plan

## Goal
Implement a "simultaneous ringing" capability that, when specific inbound addresses receive an INVITE, immediately originates parallel calls to a configured set of downstream contacts, completes the dialog with the first positive response, and cancels the remaining forks.

## Functional Requirements
1. Define policy rules that map called addresses (e.g. Request-URIs, DIDs, user@domain targets) to one or more downstream contact URIs that should ring simultaneously.
2. Expose an administrative workflow to manage these broadcast rules (list, create, update, delete) alongside existing user-directory management.
3. When an INVITE targeting a broadcast-enabled address arrives, fork the request to all configured contacts without waiting for sequential responses.
4. Relay provisional responses (1xx) from any fork downstream, while continuing to watch for final responses.
5. Upon receiving the first 2xx response, establish the session with that branch and immediately cancel all other outstanding forks; ignore any late 2xx responses by sending a BYE/CANCEL as appropriate.
6. If every fork fails (non-2xx final responses or timeouts), aggregate the best failure response and return it to the caller.
7. Ensure retransmissions and CANCEL processing remain RFC 3261 compliant across the new multi-branch behaviour.

## Technical Considerations
- Keep the transaction layer focused on single upstream/downstream pairs; have the transaction user (proxy core) instantiate and track multiple client transactions when a broadcast rule applies.
- Introduce policy lookup hooks in the transaction user (or dedicated routing component) so INVITE handling can retrieve broadcast targets before forwarding upstream.
- Provide a mechanism in the transaction user to signal cancellation of sibling client transactions when one fork answers, propagating CANCEL requests and cleaning up transaction state.
- Record which fork produced the accepted dialog so responses and BYEs are routed consistently.
- Persist broadcast rules in the SQLite user directory or a companion table to keep configuration consistent with existing admin tooling.

## Task Breakdown
1. **Data Model & Storage**
   - Design SQLite schema additions for broadcast groups (tables for rules and associated contacts).
   - Implement CRUD methods in `sip/userdb` to manage these records.
2. **Admin Interface**
   - Extend `cmd/user-web` to surface pages/forms for the broadcast mappings, reusing authentication and templates.
   - Wire new handlers to the CRUD APIs and add integration tests if applicable.
3. **Routing Policy Integration**
   - Load broadcast rules during SIP stack initialisation and expose them via a lookup service consulted by the proxy core.
   - Update configuration docs to describe how addresses map to broadcast lists.
4. **Transaction Layer Enhancements**
   - Allow multiple concurrent client transactions per server transaction, maintaining per-branch state and correlating responses.
   - Implement cancellation logic that triggers when a winning 2xx arrives and when downstream sends CANCEL.
   - Ensure retransmissions and failure aggregation behave correctly with multiple forks.
5. **Proxy Core Adjustments**
   - Modify INVITE handling to generate forks according to the broadcast policy and to pass cancellation signals back to the transaction layer when necessary.
   - Propagate the winning response downstream and suppress late answers.
6. **Testing**
   - Add unit tests covering broadcast rule storage and lookup.
   - Create transaction-layer tests that simulate multiple forks, verifying first-answer wins and cancellation of the rest.
   - Add proxy-level tests ensuring configuration-driven broadcast routing behaves as expected.
7. **Documentation Updates**
   - Update `design.md` and `requirements.md` to describe the broadcast ringing capability and administrative workflow.
   - Refresh user-facing README instructions as needed.

## Open Questions
- Should broadcast targets be limited to SIP URIs already present in the registrar bindings, or can they be arbitrary URIs?
- How should prioritisation or weighting be handled if multiple contacts return different error codes (e.g. choose lowest status, prefer 6xx over 5xx)?
- What timeout governs cancellation when no forks respond—reuse existing transaction timers or introduce broadcast-specific tuning?
