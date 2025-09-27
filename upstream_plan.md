# Upstream Server Support Plan

## Requirements
- Allow the proxy stack to operate without a fixed upstream hop so it can terminate routing decisions locally.
- Route requests targeting locally managed domains directly to the registered contacts for those users.
- When no dynamic route is available, fall back to resolving the Request-URI host via DNS/IP lookup before considering a configured default upstream.
- Maintain compatibility with the existing behaviour when a default upstream address is configured.

## Proposed Functional Changes
1. Make the `--upstream` CLI flag optional and treat it as a default/fallback hop instead of a mandatory setting.
2. Track the set of domains and user contact templates loaded from the user database so the stack knows which requests it can terminate locally.
3. Resolve upstream destinations per message inside the stack by consulting registrar bindings, static contact templates, or Request-URI hostnames.
4. Extend the SIP stack logging and configuration docs to reflect the new routing behaviour.
5. Exercise the routing decisions with unit tests that cover registrar-based routing, static directory contacts, direct IP targets, and default-upstream fallbacks.

## Task Breakdown
- [x] Update configuration handling (constructor, CLI) so the upstream address becomes optional.
- [x] Store user directory data (domains and contact templates) inside `SIPStack` during startup.
- [x] Implement per-request upstream resolution with registrar/static contact/DNS fallback logic.
- [x] Adapt the upstream sender loop to use the new resolution helper and improve diagnostics.
- [x] Write unit tests covering the new resolution paths.
- [x] Refresh `design.md`, `requirements.md`, `README.md` to document upstream-server behaviour.
