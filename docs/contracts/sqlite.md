# Regate SQLite durability contract (schema version 5)

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

`Store.Backup(path)` (and `Store.BackupVerified(path)`) use
`VACUUM INTO` to produce a consistent standalone SQLite snapshot while the
source database is WAL-backed. The snapshot is created in a new private
(`0700`) staging directory, set to `0600`, synced and **verified before
publication**. Publishing via a no-overwrite hard link is atomic: an
existing destination, including a dangling symlink, is never replaced.
Failures clean the staging directory without exposing an incomplete final
backup. The staging directory and final path must share a filesystem.

`VerifyBackup(path)` checks SQLite `PRAGMA integrity_check` and
`foreign_key_check`, the expected current schema, nonempty and
digest-consistent immutable HCL revision history, effect-current to
latest-event projection consistency, and basic task/queue/grant table
presence. It returns a non-secret manifest (revision ID, table counts,
file size and SHA-256). It neither runs migrations nor invokes task
recovery. Files with unsafe permissions or symlinks are refused.

Offline verification is available without a running Regate service or admin
bearer:

```sh
go run ./cmd/regate -verify-backup /secure/backups/regate.sqlite
```

Store the reported SHA-256 in an independently protected backup inventory.
A verified SQLite file is **not** proof that remote effects were reconciled,
or that the external `REGATE_GRANT_KEYS_JSON` /
`REGATE_QUEUE_KEYS_JSON` encryption keys are available for restore.
Keep the necessary key versions in a separately protected KMS/secret backup,
and test disaster recovery in an isolated environment. Filesystem snapshots
of only the main DB file while WAL is active are **not** adequate.

The snapshot preserves any active SQLite execution-leader lease as of backup
time. A restored copy must **not** start external execution while the
original worker may still be alive or its lease is valid. An observer may
inspect the copy without taking leadership. Only after the original
environment has been fenced/stopped and the copied lease has expired may
the new worker promote itself; takeover then marks interrupted tasks
`failed` and dispatched but unconfirmed effects `unknown`. Merely
restoring to another state directory is **not** remote-service fencing.

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

## Encrypted bounded FIFO waiting queue (schema v4)

Schema v4 adds `task_queue`, with an ordered internal sequence, unique task
reference, queue-envelope metadata and AES-256-GCM ciphertext of canonical
JSON task arguments. The metadata is authenticated as associated data:
task ID, owner digest, original revision, surface/tool, creation time, TTL,
original scopes digest, redaction requirement and key ID. The source
configuration remains HCL; queued input is **never stored in a plaintext
task record, trace or event journal**.

`REGATE_QUEUE_KEYS_JSON` contains a separate environment-only encryption
keyring (distinct from `REGATE_GRANT_KEYS_JSON`):

```json
{"active":"k2","keys":{"k1":"<base64-32-byte-key>","k2":"<base64-32-byte-key>"}}
```

Only if `regate.max_queued_tasks` is set to a value above zero may an
overloaded task be accepted for delayed execution, and only with this
keyring available. Default queue limit: **0 (disabled)**. Maximum: 4096.
The legacy path remains immediate rejection when over capacity. A deferred
task is internally `queued` and is exposed as MCP `working` with a
waiting message. The queue is bounded transactionally in SQLite; an overfull
or unencrypted queue fails before a task handle is issued. OAuth-delegated or
scope-bearing tasks cannot be deferred without a distinct authorized
capability grant — a bearer must never be reused after expiration.

The internal `ClaimNextQueued` operation picks an eligible owner using a
persisted least-recently-served rotation, then selects that owner's oldest
entry, atomically moving its task to `working` and deleting the encrypted queue
entry before effects may be dispatched. Unclaimed entries survive clean
restarts and leadership takeovers. An already claimed but interrupted task
becomes `failed` as usual; it cannot be automatically repeated after an
unknown external effect. Expired tasks are failed without dispatch, and
cancellation deletes the sealed payload in the same SQL transaction.
Failed authenticated decryption stops the claim and preserves evidence.

The service dispatcher now claims entries automatically while it holds
the SQLite execution lease. It checks capacity before each claim and uses
the task's original immutable HCL revision and encrypted original inputs.
The dispatcher runs at startup even after the current HCL disables new queue
admission, provided old entries remain. Slot release and periodic wakeups
allow further per-owner FIFO claims. Cancellation removes queued ciphertext and
prevents dispatch; a checked-out task carries a SQLite working-state guard
at MCP and HTTP side-effect boundaries.

A queue can be enabled only if the dedicated encryption keys are available;
the service fails closed on startup or activation otherwise. OAuth-protected
and delegated tasks remain ineligible for deferred storage, because their
user grants cannot be inferred from a past access token. An already claimed
task interrupted after a crash is failed rather than blindly replayed.

This is an operational **bounded encrypted per-owner FIFO queue**
for nondelegated work, not an identity-verified fair scheduler or a general
exactly-once executor. Corrupt or undecryptable entries block the queue pending operator
inspection, rather than being silently skipped. Production priority
policies, external remote fencing, secure key lifecycle and chaos/load
verification remain required. Ciphertext in WAL/backups needs retention
controls.

## FIFO precedence at admission (PR #33)

The active slot and queue depth are inspected under the **same SQLite write
transaction** when a new eligible task arrives. If an older encrypted entry
exists, the new task joins behind it even when an active slot has just become
free but the dispatcher has not yet claimed the head. If that bounded queue
is full, the newcomer is rejected rather than barging ahead. Disabling
`max_queued_tasks` in a later HCL revision stops **new** queue admissions
and also rejects otherwise-eligible fresh work until the previously
committed backlog drains; accepted older tasks retain their place.

This preserves an earlier accepted backlog before newly admitted work
at the admission boundary. Dispatch uses per-owner FIFO with alternating
owners, **not** a single global FIFO across different owners. It does not
schedule OAuth-protected jobs with expired subject tokens, nor claim
identity-verified weighted fairness or strict order of completion once
parallel workers are enabled.

## Durable unweighted owner fairness (schema v5 / PR #34)

Schema v5 introduces a monotonic `task_queue_clock` and a bounded
`task_queue_turns` table for the owners that still have queued work. The
claim transaction selects the owner whose last served turn is oldest;
among that owner's jobs it chooses the smallest queue sequence. An owner
without a previous turn is treated as waiting longest, ordered by the
earliest queued sequence. This yields deterministic, unweighted
round-robin **between owners** and strict FIFO **within one owner**.
The turn update, task claim, ciphertext deletion and capacity transition
commit together, so a restart does not reset the rotation. When the last
waiting task from an owner is claimed, cancelled or expires, its turn row is
deleted to prevent unbounded scheduling metadata growth.

A separate `regate.max_queued_per_owner` limit (default 32, range 1–4096)
caps each owner's waiting backlog. Both this and the existing global queue
limit are checked under the same write transaction, and overflow is
rejected *before* persisting task input. A different owner may continue
to queue even after one owner reaches its quota.

Owner is the Regate task-owner digest, not a proof of real-world identity:
if public clients can create many bearer aliases, the quota is not
anti-Sybil protection. The release still requires authenticated principal
identities, quotas across all entrypoints, advanced fairness/priority
policy, remote adapter fencing and operational load testing. This
milestone does not add a default priority order or guarantee that work
completes in the exact dispatch order.

## Backlog precedence for resumed checkpoints (PR #35)

Resuming a previously suspended task is another request for an active
execution slot. Both `ClaimTaskContinuation` and the durable OAuth-grant
`BeginTaskGrantContinuation` barrier now check the encrypted queue
depth in the **same SQLite transaction** as capacity and task-status
validation. If any already accepted entry is waiting, a resume receives
`ErrTaskBacklog`, even if a slot has just become free. The dispatcher
must claim and run the waiting work first.

A refused OAuth-grant continuation does **not** move the task to
`working`, decrypt or rotate the refresh token, change grant generation
or mutate the suspended checkpoint. Once the older backlog is drained,
normal authorization and grant refresh may proceed. This closes a
scheduler-precedence gap while retaining the rule that no uncertain
external action may be blindly retried.

The backlog gate is not a priority system: urgent recovery requires an
explicitly modeled operator policy or a separate authenticated
priority lane rather than an accidental bypass.

## Bounded encrypted queue key rewrap (PR #36)

Queued input is encrypted under the active key in the
`REGATE_QUEUE_KEYS_JSON` environment keyring. For operational key rotation,
keep the previous key **and** the newly provisioned key loaded, set the
new key ID as `active`, then restart the Regate worker so its new process
loads both key IDs. Do not put the key bytes in HCL, MCP arguments or SQLite.

The authenticated admin endpoint `regate_queue_key_usage` (or
`GET /api/queue-key-usage`) reveals only the active key ID and a count
of currently queued ciphertext rows per key. To rotate outstanding entries,
call `regate_queue_rewrap` (or `POST /api/queue-rewrap`) repeatedly.
Each call rewraps at most 32 **still-queued** records in one SQLite write
transaction, authenticating their original queue metadata, owner digest,
revision, TTL and scopes before decrypting and re-encrypting with a fresh
nonce and the current key. It does **not** claim, execute, cancel or
change an accepted task.

A rewrap fails closed and rolls back its entire batch if any referenced
old key is unavailable, metadata does not match the task, ciphertext fails
AES-GCM verification or leadership is lost. Key counts can change
concurrently because the queue worker may claim tasks normally. Once
`remaining_old_key = 0`, verify the usage endpoint again. Only then
may the old key be retired from the **live** keyring. Retiring a key also
means historic SQLite snapshots or WAL pages encrypted under it may
become unrecoverable; inventory backups and retention requirements first.

This procedure does **not** rotate OAuth grant keys
(`REGATE_GRANT_KEYS_JSON`). It does not erase ciphertext from historical
backups or SQLite WAL pages. Key custody, offsite recovery, KMS
attestation and cryptographic erasure remain deployment responsibilities.

## Periodic authenticated queue expiry (PR #37)

The SQLite-backed dispatcher now sweeps expired encrypted queue records
**without waiting for a working execution slot to become available**.
A bounded `SweepExpiredQueued(ctx, limit)` transaction checks execution
leadership, scans pending task metadata, and selects at most 256 expired
records. It verifies each row's owner, revision, tool, task status, TTL,
scope and original encrypted payload authenticity using the correct
AES-GCM key before changing any state.

For each verified expired record it atomically marks the task `failed`
with a generic expiry reason, deletes the sealed queued input, and removes
a now-unused owner scheduling turn. A missing decryption key, tampered
ciphertext or mismatched task metadata rolls back the entire batch;
the operator's evidence is **never silently discarded**. These are not
external effects and no remote write is replayed.

The dispatcher performs bounded sweep passes periodically (even while
the active-task pool remains fully occupied). Reclaimed queue capacity
may then be used for new waiting tasks. Encrypted queue admission now
requires an explicit positive TTL no greater than 24 hours, preventing
indefinite retention of newly accepted secret input. Already existing
legacy rows with invalid retention metadata require operator review:
the sweeper will not fabricate a TTL or silently delete them.

This is not secure erasure of historical WAL, snapshots or backups.
Real-world compliance requires an explicit retention, recovery and
key-destruction policy separate from SQLite row deletion.


## Offline, no-overwrite recovery from a verified snapshot (PR #39)

To restore, first fence/stop the original worker and isolate the old and new
environments from concurrent access to the same external resources. Copy or
retain the separately protected OAuth grant and encrypted task queue keyrings.
Prepare an **already existing, empty private** state directory (mode 0700):

```sh
mkdir -m 700 /secure/regate-recovery
go run ./cmd/regate -restore-backup /secure/backups/regate.sqlite \
  -state /secure/regate-recovery
go run ./cmd/regate -verify-backup /secure/regate-recovery/regate.sqlite
```

The offline restore path does not read the HCL configuration, require the
admin bearer, open the running Store, acquire the execution lease, or run
migrations/recovery. An explicit `-state` argument is mandatory. Symlink,
nonprivate, missing or nonempty state directories are rejected; an existing
`regate.sqlite` or other preexisting state is never overwritten.

`RestoreVerifiedBackup` verifies the archive, copies it into a 0700 private
staging directory under the target, syncs it, verifies it again and requires
an identical SHA-256 and application manifest. It publishes the 0600
`regate.sqlite` by an atomic no-overwrite hard link, syncs the parent
directory and removes staging files. Failed/racing publishers cannot replace
previous state. An interrupted restore may leave a verified published file
even if final directory fsync reported failure; inspect and verify before
retrying rather than overwriting it.

The copied database **retains the original leader lease**, encrypted queue,
grants and ambiguous effect journal outcomes. Starting a restored worker
before the copied lease expires fails closed. Lease expiry alone is not proof
that an old worker or an in-flight upstream write has been fenced: the
operator must independently establish resource isolation. The procedure
does not prove KMS key availability, grant validity, remote-effect
reconciliation or secure erasure of old snapshots. Those remain open for v1.


## Offline encryption-key recoverability audit (PR #40)

A structurally valid backup may retain encrypted task inputs or OAuth grant
credentials that cannot be recovered with the available keyrings. Before
promoting any restored worker, load the separately escrowed
`REGATE_QUEUE_KEYS_JSON` and `REGATE_GRANT_KEYS_JSON` into the **offline**
audit process and run:

```sh
go run ./cmd/regate -verify-backup-keys /secure/backups/regate.sqlite
```

The command first performs structural backup verification, then opens the
standalone SQLite snapshot read-only and authenticates every retained queued
AES-GCM payload and active/grant-refresh ciphertext against its original
owner, revision, scope and resource metadata. A failed/missing/wrong key or
altered bound metadata rejects the audit without returning secret contents.
Only anonymous counts and the verified snapshot digest are printed; the
source HCL, ciphertext, queued arguments, refresh credentials and key IDs
are never returned.

Revoked or `needs_reauth` grants with erased ciphertext are non-executable
and do not need the retired keys. A grant left `refreshing` after an
ambiguous IdP rotation is **also non-executable** regardless of whether old
ciphertext authenticates; the operator must reauthorize it. Decryption
proves only local ciphertext/key compatibility, **not** present IdP
acceptance, current owner authority or right to replay an unknown effect.
The audit neither starts a Store nor runs migrations, OAuth exchanges,
continuations or MCP/HTTP actions.

Perform this audit against the *offline* snapshot, never treating an active
WAL-backed database file as a consistent backup. Use the ordinary
`-verify-backup` separately for structural validation and digest inventory.


## Bounded OAuth grant-key rotation (PR #41)

Operator management provides `regate_grant_key_usage` and
`regate_grant_rewrap` on the authenticated administrative MCP endpoint,
and `GET /api/grant-key-usage` / `POST /api/grant-rewrap` on authenticated
administrative REST. These operations do not accept or return token/key
material. Key usage returns aggregate ciphertext counts per key ID and a
`blocked` count for non-quiescent, expired or indeterminate grants.

To rotate live `REGATE_GRANT_KEYS_JSON` credentials, preserve old and new
32-byte encryption keys in the environment keyring, set the **new** key ID
as active, and restart the worker to load both. Use the management tools
to rewrap up to 32 quiescent suspended grants per call. Each authenticated
AES-GCM old-key decryption and reseal under the new active key increments
the credential generation, fencing stale continuations; task owner,
revision, issuer, audience and scopes remain unchanged. All batch writes
commit together under the SQLite execution-leader lease. A corrupted
credential or binding rejects the entire batch.

A `working` task, a grant currently `refreshing`, a credential beyond
its expiration, or any unsuspended task **cannot be rotated**. Such
ciphertext is reported as `blocked`; outstanding old-key ciphertext is
always reflected in `remaining_old_key`. A `needs_reauth` or revoked
record with erased ciphertext does not need the old key. Never force
rewrap of an ambiguous refresh, replay a refresh request, or treat an
admin key rotation as delegated permission.

Before retiring an old key from the **live** ring, require stable
`remaining_old_key=0` after all eligible grant batches. Check separately
for archived SQLite snapshots/WAL pages that still depend on old keys;
do not destroy historical keys until the retention and external KMS
custody policy permits it. This does not attest the IdP's current grant
validity, remote revocation or external resource fencing.


## Mandatory restored-state execution quarantine (PR #42)

Every `regate -restore-backup ARCHIVE -state EMPTY_PRIVATE_DIR` now writes a
durable `.regate-restore-quarantine` sentinel before publishing
`regate.sqlite`. `Store.New`, lease acquisition and effect boundaries
refuse a quarantined state. This prevents an old snapshot from replaying
queued tasks or OAuth grants executed/revoked **after** the backup.
It supersedes the earlier note that copied-lease expiry alone was a
sufficient precondition for starting the restored worker.

After explicitly fencing the old process, all its upstream resource access,
and in-flight external effects, load the original encrypted keyrings.
Use the offline key audit, then acknowledge the exact snapshot digest:

```sh
go run ./cmd/regate -verify-backup-keys /secure/regate-recovery/regate.sqlite
go run ./cmd/regate -acknowledge-restore \
  -state /secure/regate-recovery \
  -snapshot-sha256 ORIGINAL_BACKUP_SHA256 \
  -operator recovery-operator \
  -fence-evidence "Original Regate process and remote resource writes were fenced, and unknown outcomes will be reconciled."
```

Acknowledge is a deliberately destructive recovery barrier, NOT a blind
resume. It requires the matching pristine snapshot, an offline private
state directory, authenticatable retained ciphertext, and an expired
copied execution lease. In one SQLite transaction it marks **all**
`queued`, `working`, and `input_required` historical tasks failed,
discards queued ciphertext and suspended checkpoints, and revokes every
historical active/refreshing OAuth grant (incrementing generations and
erasing live ciphertext). Effect journals and revision history are
preserved for independent reconciliation. No HTTP or OAuth calls occur.
The result is reverified and a `0600` acknowledgment receipt containing
only actor, SHA-256 digests, timestamp and anonymized counts is synced
before the quarantine marker is removed.

The operator's `-fence-evidence` is an **unverified human assertion**
stored only as a digest. It does not prove remote resource isolation,
unknown effect outcomes, historical key destruction or that future user
requests are safe. Unknown historical writes must be reconciled with
upstream readback before submitting fresh changes. If acknowledgment
fails after SQLite mutation, **do not manually remove the quarantine
marker**: create a new empty state directory and repeat the verified
restore/acknowledgment from the original immutable backup. Never
automatically resume historical queued requests from a DR snapshot.

### Draft source CAS and activation consistency

Each draft response exposes `source_digest` as the **full hex SHA-256**
of its exact persisted HCL source, independent of the shorter revision ID.
A management caller can supply `expected_source_digest` to
`regate_draft_patch`, `regate_graph_patch`,
`regate_attribute_patch`, `regate_attribute_add`, or
`regate_activate` to reject a stale reviewed source.

The graph/attribute editors always perform a SQLite
`UPDATE ... WHERE id=? AND source=?`, so concurrent edits cannot overwrite
one another, even if the caller did not send an explicit digest.
The full-source replacement API remains unconditional when invoked by
an older client **without** a precondition; use the digest to avoid
lost updates. A conflict never mutates the draft.

Crucially, `Service.Activate` validates one exact draft snapshot,
prepares the corresponding MCP catalog and commits **only if those
same source bytes remain in SQLite**. An edit between compilation and
the activation transaction fails closed. This prevents divergence
between the activated source and in-memory enforcement/catalog.
The optional activate digest pins the operator's reviewed snapshot.
The visual editor refuses to reserialize a graph against a draft
changed in HCL or another session. This is source snapshot concurrency,
not automatic merge or provider execution fencing.

### Durable trace and effect diagnostic masking

New trace messages and free-form effect and resolution reasons are
bounded and scrubbed with the shared diagnostic redactor immediately
before their JSON records are inserted into SQLite. The storage API
does not rely on every runtime, MCP or adapter call site remembering
to mask its own error. Existing effect intent digests, evidence
digests, status and resource identity are preserved verbatim to avoid
corrupting audit identity. Known bearer-token, secret-assignment and
email patterns are removed from new raw records.

This is layered best-effort redaction, **not** data-flow taint
attestation or retrospective migration of older forensic records.
Other free-text metadata, external KMS/backup key custody and the
complete sensitive-data privacy matrix remain separate release gates.

### Trace inspection projection and query limits

SQLite retains the newest **2000 trace records**, ordered by increasing
write sequence. The admin REST endpoint `GET /api/traces` returns
newest-first JSON and accepts optional exact `run_id` and `status`
match selectors plus `limit` (1–2000, default 2000). Limits apply to
the matching subset **after** scanning the bounded retained projection;
unrelated recent records cannot crowd out a run being investigated.
Unknown/duplicate selectors, oversized or malformed inputs and invalid
limits are rejected. The management MCP `regate_pipeline_trace`
uses the same exact run filter semantics.

Console trace queries never reload or overwrite a pipeline editing
session. The 2000-record projection and scrubbed diagnostics are not
a substitute for durable distributed telemetry/audit storage; data
older than the retention cap is intentionally unavailable.
