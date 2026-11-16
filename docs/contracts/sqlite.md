# Regate SQLite durability contract (schema version 2)

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
is absent or expired. No task is automatically replayed after takeover.
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

### Not yet supported by this milestone

- Configurable verified external read-back adapters and policy-based
  compensation for arbitrary resources. A typed readback verification
  interface, stable effect IDs and operator-resolution journal now exist
  (PR #21), but verified adapters must still be supplied per resource.
- Multi-instance fair scheduling, crash-safe preemption and remote adapter
  fencing of effects already dispatched. Local SQLite leader leasing and
  epoch-based refusal of new operations are implemented (PR #22).
- Encryption-at-rest, KMS rotation and secure erasure of historical PII/secret
  material (the SQLite file and backups must be protected by filesystem
  permissions and deployment controls).
- Durable audience-restricted OAuth capability grants for asynchronously
  resumed work; raw bearer tokens must not be persisted.

These are part of issue #6 and the release/security acceptance gates. SQLite
migration alone does **not** prove the entire Regate v1 durability contract.
