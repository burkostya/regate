# Regate release acceptance / implementation status

**Target scope (user-approved):** full first release includes HTTP-only MCP client/server, editable virtual tools/resources/prompts, HCL2 declarative config, typed transformations/CEL, DAG branches and compositions, HTTP/MCP hooks including agents/PII/Decision API, policies, revisioned management MCP and skills, SvelteKit+Svelte Flow visual editor, async tasks, diagnostics, security and tests. **Do not silently split or drop these features.**

**Current deliverable:** a Go engineering prototype with HCL2/CEL typed IR and JSON Schema port contracts (PRs #11 and #12), official-SDK upstream HTTP client (#13) and an incoming SDK migration (#15), plus SvelteKit frontend source. NOT complete v1. This distinction is important when planning installation or deployment.

## Implemented and locally verified

| Area | Status | Evidence |
|---|---|---|
| Go CLI + local HTTP server | Implemented | `go build`, smoke test |
| Upstream HTTP MCP client via official Go SDK v1.8.0 | Partial implementation: HTTP only; per-operation sessions and SDK protocol negotiation; no production OAuth | Official SDK modern/legacy test servers and integration tests |
| Public tools with direct source and DAG pipeline | Implemented subset | `TestEndToEndMCP`, `TestMCPDirectProxyPreservesContent` |
| HTTP Decision API / PII transform / MCP agent hook | Implemented subset | `TestEndToEndMCP`, deny test |
| `on_request`, `before_call`, `after_call`, `before_response`, `on_error` hooks | Partial implementation | Go code and integration paths |
| HCL2 parsing/cty, CEL, closed block schemas and typed IR | Implemented subset in PR #11; Go GitHub CI passed | `internal/config`, `internal/expr` and negative/source-preservation tests |
| DAG conditional edges/parallel waves/join | Basic implementation | Code path; incomplete formal/race coverage |
| Prompt/resource publishing and proxy resource read | Partial implementation | Server method path |
| Management MCP and REST endpoints | Implemented subset, official SDK inbound implementation (#15; conformance pending) | Auth + draft/activate/conflict/rollback, SDK client interoperability and skills tests |
| Immutable revision history + drafts | SQLite transactional implementation (PR #20); complete v1 validation pending | Atomic conflict/rollback/restart, cross-connection and legacy-import tests |
| Dry-run with explicit mocks | Partial implementation | REST API path |
| Effect journal (`dispatching`, `confirmed`, `unknown`) | SQLite MCP/HTTP admission, positive-only MCP readback and explicit one-time operator-approved MCP compensation. Provider-verified semantics pending (#6) | Restart, no-replay, admin auth, readback/compensation success/unknown and concurrency tests |
| Async MCP tasks, cancellation, checkpoint and resume | Partial; live OAuth token exchange for async work and owner-bound reauthentication on resume without persisting bearer. Autonomous durable grants and crash resume remain unimplemented | OAuth delegation and restart/fail-closed integration tests |
| Subscription change notification | Implemented subset | SSE activation test |
| Outbound HTTP DNS/IP safety guard | Implemented initial fail-closed guard | `internal/netguard` unit tests (DNS rebinding / mixed DNS / loopback) |
| Admin skills + `skills/list`, `skills/get` / `skill://` resources | Implemented subset | Admin skill test |
| SvelteKit + Svelte Flow editor | **Type-check and build verified** | GitHub Actions npm install/check/build passed; browser E2E remains open |
| Go race tests, vet, build | **Passed** | `go test -race ./...`, `go vet ./...`, `go build ...` |

## Unfinished functionality required for full v1

1. Finish HCL2/CEL/typed-IR acceptance (#3): cross-port JSON Schema typing, runtime validation and opaque-draft source-safe patches exist, but arbitrary JSON Schema subsumption and browser-level editor round trips remain future work.
2. Verify end-to-end protocol conformance and extensions against real servers and clients. Incoming public/admin standard MCP requests use the official SDK in PR #15; only versionless `server/discover` and task-start `resultType: task` remain narrow compatibility bridges. OAuth, long-lived upstream subscriptions, cross-process task/skill interoperability and robust upstream/session reuse remain open.
3. Full JSON Schema input/output validation, compatibility checks and source-map/opaque-safe bidirectional editing of arbitrary HCL2 fragments.
4. Full composition semantics (`pipeline.call`, map, controlled retries, named ports, safe joins with effects), robust task and MRTR resume, and property-based execution tests.
5. Production-grade effect reconciliation/idempotency/compensation, backpressure and durable scheduling (no blind retries after uncertain writes).
6. Enforced data labels/taint tracking and verified PII obligations, permissions, principal identities and trust boundaries throughout every node.
7. SQLite transactions, legacy-state import and backups are implemented in PR #20. Remaining: durable execution leases/fencing, verified reconciliation/compensation, bounded scheduling and safe retention/compaction of all sensitive records.
8. OAuth JWT/JWKS or RFC7662 introspection and issuer/audience/scope-verified public surfaces are implemented in PR #18. Management remains a separate static admin bearer; delegated upstream token exchange, end-to-end taint policies, provider-specific integration, audit-ready secret management and production threat verification remain open.
9. Independent MCP resource/prompt discovery (including templates, pagination and revision-aware caching) now exists; source-safe mapping/editing, upstream change detection and compatibility diagnostics remain open.
10. Complete UI build, type checks, browser/e2e tests, full graphical node/edge/port editor, impact analysis, robust source preservation and operator approval workflow.
11. OpenTelemetry spans/metrics/export, quotas and capacity controls, full audit history, release acceptance/e2e scenarios.
12. Execute and check the formal model under TLC/Alloy, rather than keeping an unchecked design sketch.

## Acceptance criteria for complete v1

- Two external MCP servers are connected over HTTP and exposed through one curated surface.
- A virtual tool composes multiple upstream calls, branches, joins, and invokes both an HTTP Decision API, a PII scrubber and an MCP-based LLM agent.
- A denied decision prevents any protected effect or exposure. Data classified as protected cannot cross a prohibited trust boundary, including in error paths and traces.
- A lost response to an external write does not cause duplicate action; Regate read-backs/reconciles or durably awaits an operator decision.
- An admin agent can inspect, patch, validate, test, review semantic diff, activate and roll back config using only management MCP + supplied skills.
- A user can achieve the same flow in the Svelte Flow UI, preserving unsupported HCL fragments.
- Updated upstream schemas are detected, breaking changes isolated; in-flight calls retain their original immutable revision.
- Tasks survive restart, subscription notifications work, concurrency/authorization invariants pass formal and load tests.
- Standard MCP conformance, browser integration, race/security and production acceptance suites all pass.

#### Compiler milestone: port schemas and source-safe graph edits

- JSON Schema 2020-12 input/output contracts are compiled before activation.
- Surface and node contracts are checked at the external effect boundary;
  dynamic remote data is refined at runtime.
- Basic static producer/consumer incompatibilities are rejected; full JSON
  Schema subsumption, arbitrary CEL access paths and remote schema provenance
  cannot be statically proven and remain runtime/policy responsibilities.
- Structural graph patching preserves untouched nested comments, attributes
  and blocks; structural deletion removes the deleted element's own contents.
- Remote JSON Schema loading is explicitly forbidden to prevent SSRF at
  compilation. Contract schemas are embedded within HCL.

#### Official SDK upstream milestone

- Upstream tools/list pagination, tools/call and resources/read go through the official MCP Go SDK; legacy initialize and modern server/discover are delegated to the SDK. No stdio transport is supported.
- Each upstream operation has an independent SDK session. This is intentionally conservative: session initialization per operation is slower, but stale connections and hidden write retries are avoided; production connection pooling remains open.
- Outbound DNS-pinned egress and redirect blocking are preserved; token-env credentials attach only through the guarded HTTP client. This does not implement OAuth.
- Incoming standard public/admin MCP methods are SDK-backed in PR #15; custom methods for skills and task management are SDK-registered. A narrow existing wire fallback is kept for versionless Regate discovery and task-start responses (`resultType: task`) until the SDK models that extension. This does **not** prove full MCP conformance or production-readiness.

#### Official SDK inbound milestone (PR #15)

- Public/admin HTTP endpoints use official Go MCP SDK streamable HTTP with stateless requests and JSON responses, plus SDK-managed SSE `subscriptions/listen` streams.
- Tools, resources, prompts and admin skills are registered with the SDK; custom `skills/*` and `tasks/*` methods remain typed SDK extension handlers. Strict modern `Mcp-Method`/`Mcp-Name` requirements remain enforced, and admin authorization stays outside the protocol handler.
- Activation/rollback builds a new SDK catalog before committing the revision. A catalog switch leaves the old revision intact for in-flight handlers and notifies existing subscriptions through non-destructive descriptor re-registration.
- Existing Regate HTTP clients that omit `Accept` remain compatible; versionless `server/discover` retains a compatibility handler. The SDK does not currently have a typed `resultType: task` response, so task-start is still narrowly bridged through the previous dispatcher.
- Remaining limitations: no full independent external-provider conformance or browser/runtime e2e, persistent SSE session resume, completed production OAuth attestation, reconciled effect durability or tested cross-instance subscription fan-out.

#### Compiler opaque-draft editing (issue #3)

- HCL2 syntax-only `ParseForEdit` retains unrecognized attributes and nested blocks, including expressions that are not statically evaluable.
- Graph structural edits, known attribute updates and insertions preserve unrepresented opaque text byte-for-byte; edits to unknown fields or new unsupported blocks are rejected rather than silently applied or lost.
- The management draft patch accepts syntactically valid HCL while `validate` and `activate` continue to strictly reject unsupported executable HCL.
- Negative and source-preservation round-trip tests cover unknown expressions/blocks, source edits, invalid syntax and failed activation. Browser-level round trip is tracked in #8.

#### MCP full catalog and conformance milestone (PR #17)

- Admin upstream refresh captures all advertised tools, resources, resource templates and prompts in a single negotiated SDK session, with bounded pagination and strict cursor checks.
- Catalog entries include the configuration revision; activation/rollback invalidate cache and stale responses never repopulate it. Failed refreshes remain errors.
- Independent raw HTTP transport tests cover modern/legacy negotiation, standard catalog methods, skills, bad headers/methods, request-size rejection and cancellation before protected external effects.
- Remaining: external-provider compatibility matrix, extended task ownership/recovery across instances, durable SSE resumption, OAuth security, performance and load/chaos testing.

#### OAuth identity and per-object authorization milestone (PR #18)

- HCL OAuth providers and resource-scoped surfaces verify signed JWTs with pinned JWKS or opaque tokens by RFC 7662 introspection. No unsigned or unsupported JOSE token can authenticate. Issuer, audience, subject, expiration and scopes are checked, including negative signature/expiry/issuer/audience tests.
- Public resource metadata advertises issuer and scopes. Tool/resource/prompt descriptors are visibility-filtered per authenticated principal, and every invocation is checked before effects. Management token and user tokens are not interchangeable.
- By default OAuth responses (including successful asynchronous results, resources, prompts and error paths) use PII/credential masking, with an explicit scoped unmasked tool contract. Traces record only generic OAuth execution errors.
- Tasks bind creation-time scopes, redaction and original immutable config revision; renewed OAuth access tokens retain ownership by verified issuer+audience+subject, while other users and downgraded scopes cannot access them.
- Still required: dedicated data-taint/evidence policies across every pipeline/agent boundary, delegated token exchange and real OAuth providers before #5 can be closed.

#### Delegation and conservative trust boundaries (PR #19)

- RFC8693 token exchange produces a fresh target-audience access token after verifying the incoming Regate principal. Issued credentials are re-verified for issuer, target audience, subject and scopes, and never cached/forwarded as an incoming Regate bearer.
- OAuth pipeline/interceptor graphs are fail-closed before dispatch if any MCP/HTTP/agent effect lacks an explicit Restricted trust boundary; this includes conditional branches and resume paths.
- Invalid exchanges are rejected before journaling a dispatching effect. Async jobs needing transient delegated user tokens are explicitly rejected until durable grants exist in #6.
- Tested both positive MCP exchange and negative wrong-audience/subject/scope routes. Remaining #5 work includes attested taint/declassification, real IdP verification and durable delegated grants.

#### SQLite transactional durability milestone (PR #20)

- Single SQLite WAL database (FULL synchronous) is the authoritative store for immutable revisions/drafts, tasks/continuations, traces, layouts and the append-only effect events plus latest-state projection. Atomic activation/rollback and revision conflict checks hold across independent SQLite connections.
- One-transaction migration imports legacy file state without modifying the legacy originals; invalid revision digests, broken chain, malformed effect logs and missing referenced data roll back the import.
- Startup preserves potential writes as `unknown` after interrupted `dispatching`; orphaned tasks fail closed without replaying effects. Consistent snapshots use `VACUUM INTO` with no overwrite.
- Still required for #6: verified read-back reconciliation and compensation, stable idempotency/effect-contract identity, durable execution leases/multi-instance fencing, task capability grants, retention and encryption-at-rest.

#### Effect idempotency and resolution milestone (PR #21)

- SHA-256 stable intent keys per run/node/operation and canonical input digests prevent multiple SQL-admitted external writes within one logical run; concurrent SQLite writers and crash/unknown outcomes are covered by tests.
- Only `dispatching → confirmed/unknown` is accepted for an active attempt. Reconciliation provides explicit operator decisions and a typed trusted resource read-back interface with required evidence digest; a negative outcome needs authoritative consistency attestation.
- Management MCP and admin REST expose explicit operator decisions and audit the provenance separately from verified read-back. Existing event versions remain append-only.
- Remaining: real resource-specific verified read-back adapters, idempotency keys shared with upstream APIs, compensation and durable worker leases/capability grants. Do not claim exactly-once effects across independent requests.

#### SQLite execution lease and fencing milestone (PR #22)

- Schema v2 adds an exclusive execution lease with expiry, heartbeat and monotonically increasing epoch. The Regate MCP service refuses to start as a second live leader for the same state directory; new epoch promotion and abandoned-record recovery commit atomically.
- An observer may inspect SQLite but cannot mark a live leader's tasks/effects interrupted, and cannot dispatch external effects while that leader's lease is active. Old workers lose the right to perform new actions after fencing/takeover.
- Startup migration from SQLite schema v1 is tested; CLI releases the lease on normal and fatal paths. Outstanding v1 requirements include remote-effect fencing/idempotency, durable OAuth capability grants and full reconciliation adapters.

#### Positive-only MCP read-back milestone (PR #24)

- `readback` HCL blocks bind an original MCP write operation to a trusted
  read-only lookup on the same upstream endpoint.
- New SQLite effect records carry a hashed target identity. Reconciliation
  requires an exact digest of the original write arguments, a matching
  upstream endpoint and a positive exact identity match from an actual MCP
  read. Missing or negative results always remain `unknown`.
- Admin-only MCP `regate_effect_readback` and REST
  `POST /api/effect-readback` expose the workflow; no automatic retry.
- The configured `safe_read=true` is an operator assertion, not an independent
  vendor proof. No negative consistency semantics, remote idempotency/fencing,
  revision-pinned attestation, durable encrypted OAuth grants or compensation
  are provided by this milestone. Issue #6 remains open.

#### Operator-approved MCP compensation milestone (PR #25)

- A strict HCL `compensation` block maps one applied original write operation
  to an explicitly enabled inverse tool at the exact original MCP endpoint.
- The authenticated admin must supply the original intent, an actor and a
  reason. The source must be `resolved_applied` with matching intent and
  target digests, not merely `confirmed` or `unknown`.
- Every inverse is a separately journaled effect linked to the original.
  A stable inverse ID admits at most one attempt per original source even
  across concurrent admins and crashes. Unknown outcomes never replay.
- Administrative MCP `regate_effect_compensate` and
  REST `POST /api/effect-compensate` are protected by admin bearer.
- Compensation is **not** automatically executed, and a confirmed inverse
  response does not prove restoration. Provider-specific effect verification,
  remote idempotency/fencing and durable scoped OAuth grants remain open.
  Full issue #6 remains unfinished.

#### Interactive OAuth delegated async tasks (PR #26)

- Verified OAuth subjects may launch MCP asynchronous tasks that require
  target-audience token exchange, as long as the verified inbound bearer remains
  live **only in memory**. Execution is bounded by the token expiry (with
  safety margin) and the task deadline. No inbound or exchanged access
  token or refresh credential is stored in the task, checkpoint or journal.
- SQLite stores only provider, issuer, audience, hashed owner identity,
  immutable original revision and the union of required surface/tool scopes.
  Public `tasks/get`, `tasks/update` and `tasks/cancel` re-check owner
  and the original scopes on the current inbound request.
- After user-input suspension, `tasks/update` supplies a newly verified bearer
  of the same issuer/audience/subject and sufficient scopes, and the original
  revision is used for continuation. The context-free compatibility method
  refuses delegated resumes. Invalid/expired/revoked-scope or wrong-owner
  tokens cannot resume the task.
- **Not** autonomous durable OAuth delegation. Process crashes and abandoned
  in-flight tasks still fail closed; a suspended task requires interactive
  reauthorization to continue. Durable unattended encrypted capability grants,
  provider revocation enforcement, KMS/rotation, cross-process task scheduling,
  remote fencing and production provider integration remain open in #5/#6.
