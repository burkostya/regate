# Regate SQLite durability contract (schema version 3)

SQLite is the authoritative on-disk store for revisions, active selection,
drafts, graph layouts, MCP task records, suspended run checkpoints, execution
traces and the append-only effect event journal. The database is stored at
`<stateDir>/regate.sqlite`, with WAL journaling and
`PRAGMA synchronous=FULL`. The Go driver uses pure Go (no CGO).

## Execution leadership and fencing

Schema v2 adds an `execution_leases` table. A running Regate service must
acquire the single `default` lease before exposing MCP endpoints. An
independent process opening the same state directory cannot become execution
leader while that lease is alive; it may inspect stored state but cannot
dispatch external writes. Leases have a 30-second deadline renewed every
5 seconds. Each takeover increments a monotonic `epoch`; an old worker's
late heartbeat, stale `Close` and subsequent external effects are fenced.

A database observer must **not** convert another worker's live
`dispatching` effects into `unknown`, nor fail its running tasks. Startup
recovery of abandoned effects and tasks happens only when the previous lease
is absent or expired. Promotion of a new worker epoch and recovery of all
abandoned `dispatching`/ `working` records share **one SQL transaction**:
this also covers observers that opened the database before the prior lease
expired. No task is automatically replayed after takeover.
The CLI releases leadership on graceful shutdown and on fatal validation
paths after startup.

This protects a single SQLite-backed Regate deployment from common duplicate
worker execution. It is not external-service fencing: an already in-flight
write cannot be revoked at a remote service without an idempotency key or a
resource-specific fencing contract. Full distributed coordinator semantics
remain a separate acceptance gate.

## Atomicity and restart

- Each activation creates a new immutable revision and updates the current
  revision in a single SQL transaction. A draft based on another active
  revision fails with `revision conflict`, including across two processes
  accessing the same SQLite file.
- Rolling back is a new revision; old revision IDs, source and digest remain
  accessible. Historic HCL is never silently rewritten.
- Traces, suspended continuations, task state and effect changes live in
  dedicated SQLite tables. An effect event and its current-status projection
  are committed atomically. A command must journal `dispatching` **before**
  sending a potential write to an external resource.
- A crash between dispatch and confirmation leaves an ambiguous external
  outcome. Startup converts durable `dispatching` into `unknown`. It must
  **never** infer that the upstream write failed or retry it automatically.
- A task that was `working` when a process stopped is marked `failed`
  with a reconciliation diagnostic, not resumed by re-executing an unknown
  write. A task suspended for input remains checkpointed separately.

## Sealed delegated capability storage (schema v3)

Schema v3 adds a task-scoped `task_grants` table without changing existing
revision, task or effect history. It contains at most one credential per
existing delegated task. Metadata binds the grant to the original task ID,
owner digest, HCL revision, OAuth provider, issuer, Regate audience and the
SHA-256 of its original required scopes. None of these bindings may silently
expand when the configuration or task is edited.

Only explicit provisioning of a refresh credential is supported by the
storage layer. `REGATE_GRANT_KEYS_JSON` supplies AES-256-GCM keys in an
environment-only keyring:

```json
{"active":"k2","keys":{"k1":"<base64-32-byte-key>","k2":"<base64-32-byte-key>"}}
```

The current key ID, per-grant cryptographically random nonce, ciphertext,
generation and expiry are stored. The AES-GCM authenticated data covers the
entire authorization binding, generation, expiry and key ID; changing any
of them or modifying ciphertext causes decryption to fail. No raw refresh,
access or exchanged token is stored in SQLite, task records, traces, HCL or
MCP responses. A consistent `VACUUM INTO` backup contains ciphertext; the
keyring must be backed up separately with appropriate KMS/secret controls.

The one-time `BeginTaskGrantRefresh` operation changes `active` to
`refreshing` inside a SQL transaction **before** the caller contacts the
authorization server. The caller must independently verify any refreshed
issuer, audience, subject and scope before atomically rotating the sealed
credential using generation compare-and-swap. A network failure, lost
refresh response, crash or leader takeover transitions the grant to
`needs_reauth`, wipes its live ciphertext and forbids automatic replay.
Explicit revocation similarly makes the grant unusable, and a late refresh
completion cannot resurrect it. Key rotation requires both the old and new
key at rewrap time; after all rows have been rewrapped, old keys can be
retired. Historical SQLite WAL pages/backups may still contain encrypted
versions until separately retained/erased.

**Sealed grants are no longer accepted without verifying the issuing OAuth provider.**
An owner-scoped public MCP custom method reserves one initial grant attempt,
requests a rotated credential from the configured IdP and validates the
issuer, Regate audience, original user identity and task scopes before
committing ciphertext. A failed or ambiguous response stays `needs_reauth`
and cannot be blindly repeated. No current execution path consumes the
sealed grant for unattended continuation, and no crashed task is replayed.
Actual delegated scheduling, revocation checks at effect boundaries and
provider-specific recovery remain separate acceptance stages.

## Legacy file import

On the first SQLite startup, if `history.json` exists, Regate imports in
one transaction: `history.json`, `revisions/*.hcl`, `drafts/*.json`,
`tasks/*.json`, `suspended/*.json`, `layouts/*.json`,
`traces.jsonl`, and `effects.jsonl`.

Every revision source must match its recorded digest; parent links must form a
consistent chain. Critical missing/corrupt data causes the **entire import
transaction** to roll back. Malformed effect journal data is **not ignored**,
because dropping it could erase the last confirmation of a write. Old files
are left intact and are never used again as the live backend after a
successful migration. If old artifacts exist without a valid history, Regate
fails closed rather than starting an empty store over them.

Operators should make a filesystem backup *before* the first SQLite migration.
The old files are retained after a successful import for recovery/inspection;
their lifecycle is operator-managed.

## Backup, verification and limitations

`Store.Backup(path)` uses `VACUUM INTO` to produce a consistent standalone
SQLite snapshot while the source database is WAL-backed. It does not overwrite
an existing backup. An operator should verify that a backup can be opened and
read by SQLite; filesystem snapshots of only the main DB file while WAL is
active are **not** adequate.

Tests cover restart, preserved ambiguous effects, task fail-closed recovery,
consistent backup, transactional import including all state domains, corrupt
migration rollback, and conflicting writers.

## HTTP effects and stable hook steps

Effect journaling covers MCP calls, HTTP `http.call` nodes, external HTTP
interceptors and HTTP Decision hooks. A remote HTTP POST that may have
committed before the reply was lost remains `unknown`, with the same stable
run/step/endpoint identity after restart. The journal stores only digests,
not request bodies or URLs. A policy `deny` from a valid HTTP response
confirms the request's transport outcome; it does not mean Regate permitted
the protected action.

This protects against replay of the same logical step, **not** against a new
independent client request or an external system ignoring idempotency/fencing
contracts. HTTP 5xx and malformed responses remain uncertain until
authoritative read-back or operator reconciliation.

## Endpoint-bound positive reconciliation

New MCP writes bind the effect to a SHA-256 `target_digest` of their original
remote endpoint. An explicitly configured, read-only MCP lookup can resolve
`unknown` only when both that target and the canonical digest of the exact
original write arguments match the stored journal entry. The response identity
must match the original intent. Only `resolved_applied` is supported by this
MCP adapter: absence or an inconclusive read leaves `unknown`.

Imported, legacy or unbound journal entries are never retroactively assigned
a target, and can only be handled by a separately audited operator decision
or an independently verified legacy-resource adapter.

## Audited compensation attempts

SQLite journals each explicitly authorized MCP inverse as an independent
effect with `compensates` pointing to the original applied effect. Only
`resolved_applied` originals with matching target and intent digests may
start a compensation. The inverse effect ID is stable per original effect
regardless of the contract or operator selected. Concurrent admission is
atomic, so there is at most one inverse attempt.

The approval includes the asserted operator identity, approval timestamp and
a digest of the explanation (not the raw text). Unknown inverse outcomes are
never retried automatically; abandoned `dispatching` inverses become
`unknown` on lease takeover. A `confirmed` inverse denotes an observed MCP
response but does not assert the resource was restored. The original event
remains immutable.

### Not yet supported by this milestone

- The MCP adapter supports only an operator-asserted read-only positive
  identity lookup. Verified vendor-specific read semantics, authoritative
  negative consistency, HTTP readback and independently verified
  compensation semantics for arbitrary resources remain outstanding.
  Explicit operator-approved MCP inverse attempts are journaled, but
  compensating-action verification is not implemented.
- Multi-instance fair scheduling, crash-safe preemption and remote adapter
  fencing of effects already dispatched. Local SQLite leader leasing and
  epoch-based refusal of new operations are implemented (PR #22).
- Encryption-at-rest, KMS rotation and secure erasure of historical PII/secret
  material (the SQLite file and backups must be protected by filesystem
  permissions and deployment controls).
- Protected, owner-verified OAuth refresh issuance and actual unattended
  task scheduling/continuation are not yet connected to the sealed vault.
  A grant cannot currently authorize effects by itself.

These are part of issue #6 and the release/security acceptance gates. SQLite
migration alone does **not** prove the entire Regate v1 durability contract.

## Grant-bound continuation and crash semantics (PR #29)

The `BeginTaskGrantContinuation` transaction couples the original
`input_required` checkpoint, its durable task record and the matching
`active` encrypted OAuth grant. It atomically changes both the task to
`working` with `refreshing_grant` phase and the grant to
`refreshing`. This is a write-ahead authorization barrier, not an
idempotent refresh retry.

The provider's verified, rotated refresh token must reach the vault with
generation-CAS before the original immutable pipeline is continued.
A lost token-rotation response, crash during refresh or revoked grant
fails closed and cannot trigger another refresh. A resumed worker carries
only the task/grant generation in its context; the actual access bearer
is transient and each external effect checks the current SQLite generation.
Cancellation revokes the grant and stops subsequent effects.
The original checkpoint cannot run simultaneously in another worker.
An interrupted `working` task is still marked failed on leader takeover;
no automatic replay is admitted.

These mechanisms do not revoke already in-flight external calls and do
not provide an unattended durable task scheduler, provider-enforced
fencing or independently verified compensation.

## Bounded asynchronous execution slots (PR #30)

Task admission now checks the current SQLite execution leader lease and
counts `status='working'` tasks inside the same write transaction that
inserts a new task. The maximum is the active revision's
`regate.max_active_tasks` (default 32, range 1–1024). A full ledger
rejects the request with `ErrTaskCapacity` before creating a task handle
or sending external effects. Distinct callers therefore cannot overrun the
limit through a race between counting and inserting tasks.

An `input_required` task reclaims a slot with an atomic conditional
`input_required → working` transition, without overwriting concurrent
cancellation or a second resume. For grant-backed resumes, the capacity
reservation and OAuth refresh-token `refreshing` transition occur in one
transaction. Rejected transitions leave the checkpoint and ciphertext
unchanged. Task completion, failure and cancellation release capacity.

No unbounded or unencrypted waiting queue has been introduced. Overload
is explicit backpressure, not a guarantee of eventual scheduling.
Durable bounded queues, fairness, task progress recovery and production
saturation/load verification remain outstanding.
