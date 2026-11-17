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

#### Sealed delegated grant storage milestone (PR #27)

- SQLite v3 `task_grants` encrypts explicitly provisioned refresh credentials
  with AES-256-GCM from a versioned environment-only keyring. The authenticated
  data binds original task, owner, issuer, audience, provider, revision,
  scopes, generation and expiry; plaintext tokens never enter the journal,
  HCL, task record or task response.
- The grant is one-per-task with transactional admission, CAS rotation,
  explicit revoke, key rewrap and fail-closed `needs_reauth` recovery after
  an interrupted refresh attempt. Tests include wrong owner, scope drift,
  ciphertext tampering, migration, backup, concurrent provisioning, key
  retirement, revocation racing rotation and restart.
- **No external grant-issuance or unattended execution flow exists yet.**
  A sealed string is not automatically an authenticated OAuth grant. Next
  stages must verify refresh tokens against the authority, bind real
  provider-specific renewal, make revocation effective at effect boundaries,
  and schedule only proven-safe continuations after crash/restart.
- Issue #6 stays open, including provider-specific read-back/compensation
  guarantees, external fencing and full production hardening.

#### Verified OAuth task-grant provisioning milestone (PR #28)

- Public OAuth task owners may explicitly provision a **suspended delegated
  task** using `tasks/grants/provision`; this command is not exposed on
  admin MCP. Persistent SQLite admission precedes any refresh-token request
  and prevents duplicate IdP rotation when the response is unknown.
- The IdP response is independently verified for issuer, resource audience,
  same task subject and original surface+tool scopes. Only the newly rotated
  refresh credential is sealed, with bounded expiration. Public
  `tasks/grants/status` and `tasks/grants/revoke` are owner/scope-gated.
- Negative tests cover wrong owner, revoked scopes, mismatched refreshed
  subject/audience, repeated provisioning, ciphertext secrecy and revoked
  credential access. Go race/vet/build and Web CI are acceptance gates.
- **Not yet an autonomous durable grant:** a sealed credential is not
  consumed during task continuation. Provider-specific refresh rotation
  recovery, crash-safe continuation and effect-level revocation enforcement
  are outstanding. Issues #5/#6 remain open.

#### Explicit grant-backed OAuth continuation (PR #29)

- One admin-authorized resume attempt for an `input_required` delegated
  task must transactionally reserve both its checkpoint and the sealed
  refresh credential. All providers, owners, scopes and HCL revisions are
  fixed to their creation-time contracts.
- The refreshed Regate-audience token is independently verified before
  re-sealing the rotated refresh credential. Only an in-memory validated
  access token can then issue target-audience RFC 8693 exchanges, and
  every external effect checks active task/grant generation.
- Cancellation and revocation prevent subsequent protected effects. Unknown
  remote results remain unknown; failed/crashed refresh and task recovery
  do not retry automatically.
- Integration tests cover real SDK MCP input-required task, resource
  audience exchanges, authenticated admin resume, checkpoint no-replay,
  grant cancellation and crash/epoch admission.
- **Still open:** automated continuation scheduler, full process-failure
  resume/reconciliation, real-IdP interop, cross-service idempotency/fencing,
  bounded durable work queues and full production security acceptance.
  Issue #6 must remain open.

#### SQLite bounded async admission (PR #30)

- `regate.max_active_tasks` (default 32, maximum 1024) caps concurrent
  active async work globally, including user-driven resumes and grant-backed
  continuations, independently of per-pipeline node parallelism.
- Initial task admission and `input_required → working` transition use
  SQLite transaction/leader fencing with atomic capacity validation. Overload
  does **not** create a task handle, queue unlimited input, or dispatch an
  external effect. Cancellation/completion release slots.
- Regression tests cover concurrent admission, slot release, second-resume
  rejection, observer denial, bounded grant continuation, bad HCL limits
  and live MCP backend overload/recovery.
- This adds **bounded active execution and immediate backpressure**, not a
  durable bounded waiting queue or fair scheduler. Encrypted queued-input
  storage, fairness, leases across process failures, retry reconciliation
  and load-chaos acceptance remain open in #6/#9.

#### Sealed bounded FIFO waiting queue storage (PR #31)

- SQLite schema v4 `task_queue` holds canonical JSON inputs encrypted
  with a separately configured environment keyring, nonce and authenticated
  task/owner/revision/scope/TTL metadata. Per-queue capacity and FIFO order
  are enforced atomically; no plaintext arguments leak into SQLite backups.
- `regate.max_queued_tasks` defaults to zero (off), upper bound 4096.
  The internal admission and claim APIs fail closed without encryption keys,
  on OAuth/scoped inputs, corruption or missing capacity. Unclaimed entries
  persist after restart; a claimed task cannot be replayed after crash.
- Tests cover queue saturation, FIFO dequeue, cancellation/erasure, TTL expiry,
  restart/takeover, invalid key material and authenticated tamper rejection.
- **Important:** this PR is queue storage and contracts only. It does not yet
  hook the queue into user-facing task admission or automatic dispatch.
  Next milestone must implement a bounded worker dispatcher and live tests
  for recovery and HTTP/MCP task lifecycle. No v1 or #6 closure.

#### Live SQLite FIFO async task dispatcher (PR #32)

- Start a bounded dispatcher after SQLite execution leadership; admitted
  nondelegated queued work is claimed oldest-first using the original
  immutable HCL revision when an active task slot is available.
- The dispatcher resumes unclaimed encrypted work after server restart.
  Claimed tasks interrupted during execution fail closed and are not
  re-queued without explicit reconciliation; expiry and cancellation
  discard sealed inputs without external effects.
- Each claimed task carries a transient SQLite-backed status/owner guard
  into MCP/HTTP effect dispatch, preventing calls initiated after
  cancellation or lost execution leadership.
- The queue requires `REGATE_QUEUE_KEYS_JSON` on startup and activation.
  No OAuth-scoped/delegated requests are silently deferred without
  resource-specific authorization.
- Tests exercise bounded backlog, live SDK MCP dispatch, FIFO/cancel,
  encrypted backup, crash recovery and fail-closed startup without keys.
- Still outstanding: fairness per principal, priority and quota scheduling,
  provider-enforced idempotency/fencing, sensitive-data retention,
  real load/chaos/security audit and full v1 release scenarios.

#### FIFO queue admission precedence (PR #33)

- New queue-eligible tasks cannot skip previously accepted encrypted FIFO
  entries just because execution capacity opens before the dispatcher claims
  the old head; capacity and queue depth are read in one SQL transaction.
- Queue overflow is rejected rather than bypassing earlier work. Disabling
  queue admission in a later revision leaves existing accepted work intact,
  and blocks new eligible tasks until that backlog drains.
- Regression tests cover order of dequeue, freed-slot races, full queue
  handling and HCL queue-disable semantics. Per-principal fairness and
  priority queues are separate remaining issues.

#### Owner-fair bounded queue admission (PR #34)

- SQLite schema v5 records per-owner last-served scheduling turns and a
  monotone scheduler clock. Claim is an atomic unweighted round-robin
  among owners while preserving FIFO within each owner's backlog; turns
  survive restart and are removed once that owner has no queued work.
- HCL `regate.max_queued_per_owner` defaults to 32 (range 1–4096).
  Admission checks global and per-owner limits together under the
  execution-leader SQLite transaction, preventing one owner from consuming
  all slots while independent owners still have capacity.
- Regression coverage includes deterministic interleaving, owner FIFO,
  turn preservation after restart, concurrent quota contention, invalid
  HCL ranges and SQLite v1/v2/v3 migrations to v5.
- This policy uses opaque task-owner hashes. It is **not** Sybil-resistant
  for unauthenticated clients and does not yet implement weighted priority,
  per-tenant funding, authenticated provider identity quotas, or verified
  fairness under production load.

#### Suspended-task admission precedence (PR #35)

- The interactive `input_required → working` transition and explicit
  grant-backed continuation both refuse a free active slot while
  earlier encrypted queue entries are pending, using a single
  execution-leader SQLite write transaction.
- Rejected resumes leave the suspended checkpoint, task status and
  OAuth refresh credential unchanged; no IdP call or remote effect
  is made. After backlog drains, the same authorized resume can
  proceed normally.
- Regression tests cover both interactive and grant-backed cases,
  ensuring a newly available slot cannot bypass committed waiting
  work even with OAuth capability rotation.

#### Encrypted queue key rotation (PR #36)

- Management-only `regate_queue_key_usage` and `regate_queue_rewrap`
  (admin REST equivalents `GET /api/queue-key-usage` and
  `POST /api/queue-rewrap`) allow operators to inventory queued ciphertext
  key IDs and re-encrypt up to 32 queued records per request under a newly
  activated AES-256-GCM key.
- Rewrap is a leader-fenced, all-or-nothing SQL transaction, preserving
  queue sequence, owner/revision binding and task status; corrupt
  authenticated ciphertext or missing old keys block the whole batch.
- Tests cover partial-batch migration, restart without the retired key,
  rewrap rollback on corruption, lease-holder isolation, and admin-only
  key inventory/rekey endpoints without key material disclosure.
- Key-retirement is **not** archival key erasure. SQLite WAL pages and
  snapshots must obey separate retention and key-backup policies; OAuth
  grant vault rotation remains independent.

#### Authenticated expiry sweep for encrypted queued input (PR #37)

- `SweepExpiredQueued` scans the bounded SQLite queue under the execution
  lease, validates task/owner/revision/TTL bindings and verifies each expired
  input's AES-GCM authenticity **before** deleting any ciphertext.
- Expired entries are atomically marked failed and removed (including
  obsolete per-owner fairness turns). A corrupt ciphertext or metadata
  mismatch rolls back the whole bounded batch instead of dropping evidence.
- The dispatcher runs this sweep even at full active-task capacity, so
  abandoned expired jobs cannot monopolize waiting slots indefinitely.
  New deferred input must have positive TTL no greater than 24 hours.
- Tests cover bounded sweeps, saturation, no remote execution of expired
  tasks, tamper-induced rollback, ownership-turn cleanup and lease fencing.
- Historical WAL/backup retention and cryptographic erasure are still
  operational security gates. Issue #6 remains open.

#### Verified and atomically published SQLite backup (PR #38)

- `Store.BackupVerified` creates a WAL-consistent `VACUUM INTO` copy inside a
  temporary `0700` directory, forces `0600` file permissions,
  verifies it, syncs it and publishes through an atomic no-overwrite
  hard link. Failed or racing publishers cannot expose partial snapshots.
- Read-only `VerifyBackup` checks SQLite page integrity, foreign keys,
  immutable revision digest/parent chain and append-only effect journal
  projections, then returns a non-secret SHA-256/count manifest.
  Corrupt, world-readable or symlink snapshots are rejected.
- `regate -verify-backup PATH` permits offline verification without
  starting a Regate service, importing an HCL source, acquiring an
  execution lease, mutating tasks or supplying an admin bearer.
- Tests include all persisted state domains, concurrent backup publishers,
  encrypted grants/queue snapshot, deliberate revision/effect corruption,
  truncation, unsafe permissions, symlinks and offline CLI output. A copied
  *live lease* blocks premature restored-worker promotion; only after
  original worker fencing and lease expiry is fail-closed recovery allowed.
- Structural backup validation is **not** resource proof, guarantee of
  external keyring availability, independent KMS/retention compliance or
  a completed disaster-recovery drill. #6 remains open.


#### Verified offline no-overwrite restore (PR #39)

- `regate -restore-backup PATH -state EMPTY_PRIVATE_DIR` validates a
  standalone SQLite snapshot, copies it into private staging, fsyncs and
  re-verifies the identical byte digest and application manifest before
  publishing `regate.sqlite` through a no-overwrite hard link.
- Restore requires an explicit existing empty mode-0700 state directory;
  no live process, admin credentials, HCL parsing or automatic task
  execution are involved. It rejects existing state, symlinks, insecure
  permissions and corrupt/replaced archives. Tests cover offline use,
  concurrency, no-overwrite and copied leader fencing.
- This completes only the **controlled offline file restore** milestone.
  Offsite key custody and test drills, source-worker fencing, KMS,
  autonomous grant recovery, remote idempotency and vendor-verified
  reconciliation remain release gates. #6 stays open.


#### Offline encrypted recovery-key verification (PR #40)

- `regate -verify-backup-keys PATH` verifies a standalone backup and
  authenticates retained encrypted queued payloads and task grants using
  environment-only keyrings without opening a Store or doing any remote
  work. The report contains only the snapshot digest and anonymous counts.
- Missing/wrong keys, tampered queued-task metadata and grant binding drift
  fail closed. Erased revoked/reauthorization grants are not misclassified
  as executable; in-flight token refresh remains indeterminate even when
  old ciphertext decrypts.
- This establishes **local key recoverability only**. No claim is made
  about actual IdP acceptance, KMS custody, external resource fencing,
  unknown-write reconciliation, automatic restart or production DR.
  Issues #5, #6 and #9 remain open.


#### Bounded OAuth grant-key rotation (PR #41)

- `regate_grant_key_usage` / `regate_grant_rewrap` and matching
  admin REST APIs expose non-secret old-key inventory and re-encrypt
  only `input_required`, quiescent, active AES-GCM task grants.
- All selected grants are authenticated and resealed atomically in
  batches of at most 32 (internal max 64) under the SQLite execution
  lease. Generation changes fence stale consumers. In-flight tasks,
  indeterminate refreshes and expired grants remain untouched and
  explicitly count toward blocked/remaining old-key ciphertext.
- Tests cover key retirement on restart, tamper rollback, leader fencing
  and admin/public authorization boundaries. No OAuth remote refresh
  occurs during rotation. Historical backups/WAL, KMS, vendor revocation
  and recovery from ambiguous writes remain open gates for #5/#6/#9.


#### Fail-closed restored-state execution quarantine (PR #42)

- Offline restore now publishes a durable quarantine sentinel *before*
  `regate.sqlite`. Store opening, leader promotion and effect guards reject
  restored state until a separate offline operator acknowledgment.
- `-acknowledge-restore -state DIR -snapshot-sha256 SHA -operator ID
  -fence-evidence TEXT` requires matching standalone integrity/key audit
  and an expired copied lease, then transactionally fails all nonterminal
  historical tasks, discards old queued inputs/checkpoints and revokes
  restored live/refreshing credentials before removing quarantine.
- Independent event/effect evidence is retained. The raw operator
  statement, queued secrets and credential contents do not appear in the
  durable receipt. A failed acknowledgment remains quarantined and must
  be retried from an immutable clean snapshot.
- This prevents **implicit historical replay**; it does NOT prove
  physical source-worker fencing, external idempotency, an IdP's state,
  unknown-write reconciliation or production disaster-recovery readiness.


#### Deterministic joins and safety barriers (PR #43)

- Runtime joins `all`, `any` and `quorum` now evaluate the correct
  activation threshold **after** every incoming predecessor settles.
  This prevents any/quorum joins from racing still-pending policy,
  suspension or external-effect branches.
- Validation requires realizable quorums with one vote per unique
  incoming predecessor. Duplicate-vote edges and inappropriate
  quorum attributes are rejected before activation.
- Table-driven branch-vote tests and a persistent suspend checkpoint
  regression guard the barrier and preserve fail-closed non-replay.
  Full typed ports, nested pipelines and verified policy composition
  remain open under #7.


#### Fail-closed mandatory policy dominance (PR #44)

- `policy.check` nodes can opt into `mandatory=true`. Such nodes must
  provide a decision input and reference a non-builtin Decision hook;
  a missing or no-op authorizer is rejected before activation.
- The compiler checks every graph path to outbound HTTP/MCP/hook
  calls, output nodes and suspension points: all must pass through
  every mandatory policy gate, ignoring guards conservatively.
  Bypass edges, parallel branch joins and unguarded output routes
  are rejected even when execution would otherwise be valid.
- Config/property-like graph tests exercise accepted dominance,
  bypass paths, missing decisions, unsafe hooks and outputs.
  Identity and direct passthrough MCP surface permissions remain
  separate work under #5/#7; marking a policy mandatory does not
  itself prove upstream trust or external effect safety.


#### Named data ports and separated control flow (PR #45)

- Explicit `edge.kind = "data"`, `to_port`, optional `from_port = "value"`,
  `node.input_ports` schemas and CEL `ports.<name>` introduce separately
  compiled data dependencies without changing legacy control-flow edges.
- Each consumer port has one producer whose completion is guaranteed
  whenever the consumer activates; data edges never add votes to joins or
  activate nodes. PR #46 generalizes this to threshold-aware guarantees.
  Known incompatible contracts fail activation; uncertain/dynamic values
  are checked before consumer-side effects, without logging their contents.
- Invalid bindings, skipped-path bypasses, unknown ports, double producers,
  malformed data/control combinations, and runtime type mismatch have tests.
  This is a bounded port milestone, **not** full typed DAG composition:
  nested pipelines, multi-output semantics, guarded data availability,
  formal proofs and real upstream contract evidence are still open.


#### Threshold-aware completion of data predecessors (PR #46)

- DAG compiler now computes guaranteed predecessor completion over control
  activation thresholds (ordinary any, join all/any/quorum), rejecting CEL
  `nodes.*` and declared port dependencies which may be missing on a viable
  activated branch.
- An all-join may safely bind data from both completed producers; a quorum
  join retains only producers shared by every winning threshold subset.
  Compile-time tests enumerate threshold/source combinations and runtime
  exercises parallel data binding and conditional skip behavior.
- Guard and remote resource semantics remain conservative; this is not
  authorization dominance, multi-output typing, nested pipeline execution or
  formal model-checker proof.


#### Bounded CEL map (PR #47)

- Pure `map` nodes compile `items` and `mapping` CEL once, require
  `max_items` in 1–64, validate every item against an optional input JSON
  Schema and the aggregate result against the output JSON Schema.
- Per-item evaluation is synchronous, cancellable and effect-free; the
  original control DAG and guaranteed predecessor analyses remain in force.
  No partial output is exposed on item failure or bounds overflow.
- This is deliberately **not** effectful map/subpipeline execution, bounded
  retry, isolation across iterations or complete full-v1 DAG semantics.


#### Pure named pipeline composition (PR #48)

- New `pipeline.call` node binds object arguments to a named callee with an
  input entry and exactly one output. Both invocation and callee schemas
  are validated, and missing conditional outputs fail closed.
- Compiler checks the transitive call graph for unknown targets, recursion,
  depth >8 and any effectful/suspending/Decision step in a callee.
  Parent/child output tracing has a derived stable subrun ID.
- This pure composition milestone does not implement effectful nested
  pipelines, durable subpipeline checkpoints, per-call resource budgets,
  or full #7/#10 v1 acceptance.


#### Explicit validated MCP structuredContent port (PR #49)

- A `mcp.call` may declare `output_ports.structured` JSON Schema and bind
  that port to a named consumer input via `edge.from_port="structured"`.
- Producer and consumer schemas are statically compared when possible and
  both checked at runtime; missing structuredContent or post-interceptor
  invalidation fails closed before downstream effects. Dry-run supports
  explicit `mock_structured` for validation regression tests.
- Ordinary value ports stay backward compatible. Multi-result envelopes,
  remote schema attestation and full MCP content/metadata transformation
  contracts remain open #7/#10 work.


#### Explicit nullable optional ports (PR #50)

- A `node.input_ports.<name>` wrapper with `optional=true` and a JSON
  Schema accepting null allows selected data sources to be absent after a
  skipped control branch. The separately compiled CEL `port_present.<name>`
  distinguishes skipped from a completed source yielding JSON null.
- Optional sources must be control ancestors, have a fully settled state and
  retain runtime schema checks for completed data. Mandatory/undeclared ports,
  missing MCP structured results, and pending sources still fail closed.
- No implicit optionality, policy bypass, or change to join activation
  thresholds. Multi-output effectful composition remains open.


#### Explicit effectful pipeline.call (PR #51)

- `pipeline.call.allow_effects=true` enables named effectful callees with
  deterministic derived subrun IDs and scoped durable MCP/HTTP/hook effect
  identities, without changing ordinary pure-call behavior.
- Parent mandatory Decision gates treat effectful calls as observable sinks;
  pure callers cannot conceal nested effects and no nested `suspend` is
  admitted. Unknown, already-dispatched writes cannot be retried in the same
  logical child subrun; subrun traces link to the parent.
- This is a fail-closed, opt-in execution boundary, **not** nested
  checkpoint/resume, a provider idempotence guarantee, or full #7 release.

- Security review closes the former top-level-only gap: Restricted-data
  destination trust and async delegated-OAuth requirements recursively inspect
  nested child calls. Invalid/missing runtime targets fail closed.
