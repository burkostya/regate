# Two independent MCP servers: local read/write acceptance stand

This stand starts **two different OS processes** (alpha and beta) running
the official Go MCP SDK over loopback HTTP. Each has its own private,
fsynced JSON state file and write-attempt counters. It never connects to
a cloud account and must never be used for production data.

## Automated acceptance

Run from repository root:

```sh
make test-mcp-stand
```

This command runs `TestTwoProcessMCPStand` with Go's race detector. The
test starts two test-binary subprocesses with independently selected ports
and disposable state directories, then launches the real Regate HTTP
gateway with `examples/two-mcp-stand.hcl`.

The acceptance exercises the following:

1. Different PIDs, ports, state files and curated catalogs (tools,
   resources and prompts) for alpha and beta
2. Normal immutable writes, reads, and a cross-upstream alpha-read / beta-write
   typed DAG gated by an actual HTTP Decision API; denied and missing
   records must not cause a beta write
3. A special `fault: "drop_after_commit"` write that fsyncs the provider's
   state and drops the TCP connection before delivering an MCP reply
4. Durable Regate effect state `unknown`, exactly one remote write attempt,
   refusal to reconcile using a changed intent or wrong endpoint,
   and successful **positive-only** readback via authenticated management REST
5. A valid MCP `isError=true` returned **after** a fsynced write: Regate
   records `unknown` and verified readback resolves it without redispatch
6. Killing alpha, restarting it from the same state directory and reading
   back its committed record and write counters (beta is unaffected)

CI executes the stand separately and as part of `go test -race ./...`.
The same provider is available as an ordinary CLI for manual reproduction.

## Composed v1 scenario on the same providers

Install the web dependencies and Playwright Chromium (see
[the browser setup](../../README.md#browser-end-to-end-acceptance-test)),
then run `make test-v1-full` from the repository root. It builds the
production console and runs the complete local scenario from scratch. The Go
test provisions alpha and beta on separate loopback ports with disposable state and loads
[`examples/v1-acceptance.hcl`](../../examples/v1-acceptance.hcl) in the Regate
HTTP service and starts local JWKS, HTTP redaction and SDK MCP agent handlers.

The cases use a signed test bearer and one published MCP tool
(`publish_summary`). The same typed graph requests a Decision API approval,
reads synthetic notes from alpha, scrubs them, calls the MCP agent and joins
its summary with a separate label branch before writing to beta. The test
checks the exact redacted record in beta, the active revision in the run's
trace, and the absence of the synthetic email and bearer in responses and
management diagnostics.

- **Confirmed:** The caller receives the record ID and the journal records
  a confirmed beta write.
- **Lost reply:** Beta commits before dropping the reply. MCP reports a tool
  error and the effect remains `unknown` after reopening the gateway HTTP
  service on the same SQLite state and after rejected readback with a
  mismatched intent. Authenticated, token-bound readback marks
  it `resolved_applied` and adds an audit entry without another beta write.
- **Refusals:** A valid bearer without the tool scope dispatches no effects.
  A denied policy reaches Decision only, and a failed scrubber reaches alpha
  and redaction only. The test checks provider and hook counters, plus the
  absence of the synthetic failure payload in the reply and diagnostics.
- **OAuth task recovery:** A task waits for approval before reading alpha.
  The service is reopened on the same SQLite state. Another subject and an
  owner without the tool scope cannot resume it. The owner resumes with a
  fresh bearer after a new revision is activated. The task writes once using
  its original revision, verified in the stored result and run trace.
- **Authoring:** The management MCP client reads the shipped authoring skill,
  inspects the upstream catalogs, patches a draft, validates, dry-runs,
  reviews the semantic diff, activates and rolls back. A patch preserves an
  opaque HCL attribute and activation refuses it. The Chromium scenario
  performs the authoring cycle from the graph and HCL editors. It also checks
  that a stale graph projection cannot overwrite a draft with opaque HCL.
- **Revision and subscription:** A live call waits in the agent while a new
  label is activated. An existing OAuth subscription receives
  `tools/list_changed`. The waiting call writes under the original revision;
  a subsequent call uses the new label and revision.
- **Schema drift:** The unpinned typed node rejects a changed agent output
  before the beta write. The management client then activates a revision
  with the reviewed agent schema pin. When the provider changes its schema
  again, two invocations stop before calling the agent or admitting its
  effect, even with an earlier matching management catalog. A fresh call
  succeeds as soon as the fixture restores its schema.

The combined run writes `.state/v1-acceptance/latest.log` with the commit SHA,
worktree status, case results and synthetic run, task, revision and effect IDs.
Use `make test-v1-acceptance` for only the Go cases, or
`make test-v1-browser` for only Chromium. The full Go race suite and the web
browser job exercise their respective cases. Set
`REGATE_E2E_CHROMIUM_EXECUTABLE` to use an installed Chromium executable;
set `CC` if the host's compiler is named differently.

The stand verifies runtime contract rejection and pre-dispatch isolation
for pinned MCP tools. Alpha and beta are the accepted v1 durable-write
environment. [Production acceptance](https://github.com/burkostya/regate/issues/10) ·
[Durable-write acceptance decision](https://github.com/burkostya/regate/issues/106)

## Sustained mixed traffic and process failures

Run `make test-v1-soak` to exercise the same OAuth-protected graph over six
minutes of steady traffic plus outage and recovery windows. Eight parallel
clients issue read-only MCP calls, normal publishes, publishes with a lost
post-commit reply, and calls refused by tool scope, Decision or the scrubber.
The run uses ten synthetic OAuth principals across two issuers, two
independent loopback ingress proxies and a disposable SQLite database with
shared OAuth quotas enabled. The second issuer reads its own alpha instance
using one of the same subject strings. The test checks that each issuer gets
its own value for the same record ID, and that a request with the wrong
issuer is refused. It raises the disposable providers' limit to 4096 records.
Use `REGATE_SOAK_DURATION=9s make test-v1-soak` for a shorter profile or set a
duration up to `15m`. The profile runs with Go's race detector.

Two Regate CLI processes share the SQLite state directory. The standby is
started with `-wait-for-leader` before any load and is checked for readiness
only after the primary is killed. The test activates a new revision while
the standby is waiting. It kills beta, continues mixed traffic during the
outage, and restarts beta on the same state. A loopback proxy then holds a
successful reply to a designated beta write until the primary is killed.
The standby acquires the SQLite execution lease after expiry, loads the
revision published by the primary, and serves through the same proxy URL.

The test correlates every expected write with the provider record, operation
token and corresponding SQLite journal entry. Committed uncertain writes must
resolve via authenticated readback with audit evidence. Writes attempted while
beta was offline remain `unknown` when readback cannot prove a commit. All
provider writes are counted, and the responses and retained diagnostics are
checked for synthetic PII and bearer leakage.

See `.state/v1-soak/latest.log` for the Git SHA, traffic counts, p95 and maximum
client latency by traffic class, measured recovery windows and verified write
totals. This profile covers two same-host processes with one leader eligible
to serve at a time and loopback proxies. For two independent write paths, run
`make test-v1-full`. The tenant test gives each issuer separate provider
state, Decision, PII scrubber and agent endpoints, then checks overlapping
record IDs, foreign task lookup/update/cancel, refusal paths and redacted
diagnostics.

## Controlled local topology

Run `make test-v1-controlled` from the repository root with Go and a C
compiler, installed web dependencies, Docker, NGINX with SSL support,
OpenSSL, jq, curl and Chromium. Set `REGATE_CONTROLLED_NGINX` to the absolute
path of the NGINX binary if the default is a privileged wrapper. Set
`REGATE_E2E_CHROMIUM_EXECUTABLE` if Chromium is outside PATH. To shorten the
run while setting up the environment, use
`REGATE_SOAK_DURATION=9s make test-v1-controlled`.

The target starts a digest-pinned Keycloak container in development mode with
two disposable realms and service-account clients. Regate verifies their
signed JWTs against Keycloak's JWKS. Independent NGINX processes terminate TLS
on loopback ports. The Go test controls when the ingress route switches to the
ready standby, so the process crash and recovery are reproducible. The same
stand runs the built web console in Chromium and exercises Decision, PII
scrubber and agent hooks with the disposable alpha/beta MCP processes. A
separate tenant case uses synthetic tokens to deliberately exercise an
identical subject string across issuers.

After effect reconciliation, an OpenSSL process signs the verified audit
checkpoint with a disposable Ed25519 key. The script verifies the signature and
checks that a later SQLite snapshot extends that checkpoint. The report lives
at `.state/v1-controlled/latest.log`. Its `signed_audit_evidence` entry points
to the retained public key, signed checkpoint manifest and later manifest.
Temporary fixtures, private keys and database snapshots are removed at exit.

Set `REGATE_AUDIT_EVIDENCE_DIR` to that evidence directory and run
`make test-v1-audit-retention` to exercise retention using a digest-pinned
RustFS container. The test uploads the signed manifest bundle to a disposable S3
bucket with a one-day COMPLIANCE lock, adds an unretained control version and
restarts storage. The same account must be able to delete the control version
while deletion of the locked version returns HTTP 403. The test reads the
locked version by ID, compares its bytes and verifies the signature with
the public key held separately. Results are recorded in
`.state/v1-controlled/audit-retention.log`. The temporary container and its
volume are removed after the test.

This profile covers one host and a single execution leader. Target deployment
testing, external key custody, off-host immutable audit retention and security
and PII signoff remain tracked in [#10](https://github.com/burkostya/regate/issues/10).

## Running the three processes by hand

Use three terminal windows and a single disposable state root. Set the
`REGATE_TEST_ROOT` variable in each window to the same path.

```sh
export REGATE_TEST_ROOT="$(mktemp -d)"
mkdir -p "$REGATE_TEST_ROOT/alpha" "$REGATE_TEST_ROOT/beta" "$REGATE_TEST_ROOT/gateway"

# Terminal 1
go run ./cmd/regate-test-mcp -name alpha \
  -state "$REGATE_TEST_ROOT/alpha" -listen 127.0.0.1:9201

# Terminal 2
go run ./cmd/regate-test-mcp -name beta \
  -state "$REGATE_TEST_ROOT/beta" -listen 127.0.0.1:9202

# Terminal 3
export REGATE_ADMIN_TOKEN='local-test-only-secret'
go run ./cmd/regate -config examples/two-mcp-stand.hcl \
  -state "$REGATE_TEST_ROOT/gateway" -listen 127.0.0.1:9200
```

The MCP endpoints are `/mcp`, and each fixture also exposes `/healthz`.
Alpha's `/decision` is a **test-only** deterministic Decision API: valid
synthetic record IDs are allowed, IDs prefixed with `DENY-` are denied.

Both fixtures expose tools:

| MCP tool | Arguments | Effect |
| --- | --- | --- |
| `put_record` | `record_id`, `value`, optional `fault`; Regate injects `operation_token` | Create immutable test record (never overwrite) |
| `get_record` | `record_id` | Read record; not found is not authoritative absence |
| `stats` | empty object | Read received writes, committed writes, record and read counters |

Record IDs are limited to 48 safe ASCII characters; values to 256 UTF-8
bytes. There are at most 128 records per process. Both test-only fault values `drop_after_commit` and `error_after_commit`
are accepted only on a `put_record` request. The second returns an ordinary
MCP `isError=true` JSON envelope after the commit, not a broken transport.

The published Regate surfaces are `/mcp/alpha`, `/mcp/beta` and
`/mcp/workflow`. The last exposes the `copy` pipeline that reads a
synthetic record from alpha and writes the same record to beta, only
after a mandatory Decision check.

## Raw MCP smoke request

Example against the published alpha Regate surface (modern MCP headers):

```sh
curl -sS http://127.0.0.1:9200/mcp/alpha \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'Mcp-Protocol-Version: 2026-07-28' \
  -H 'Mcp-Method: tools/call' -H 'Mcp-Name: alpha_put' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"alpha_put","arguments":{"record_id":"manual_1","value":"safe test data"},"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}'
```

To simulate a lost response, use another unique record ID and include
`"fault":"drop_after_commit"`. For an ordinary post-commit MCP tool error,
use `"fault":"error_after_commit"`; the record is still written and must
be reconciled despite the application-level error reply. The client must see an error but a read
must find the committed record. Inspect the protected `/api/effects`
endpoint and reconcile by `/api/effect-readback` using adapter
`alpha_record` and the **exact original arguments**. The automated test
runs this full sequence and checks that the write is never silently replayed.

Stop all processes before removing the disposable `REGATE_TEST_ROOT`.
Do not reuse an existing Regate state directory. The fixture intentionally
requires loopback listeners and has no public authentication; running it
on a public network would be unsafe.

**Limitations:** The two processes use the same fixture implementation.
This is NOT real third-party MCP-provider attestation, proof of arbitrary
mutable-resource consistency, cross-process global fencing, OAuth/PII
certification or production load/chaos acceptance (#7/#9/#10).

### Content matching for safe positive readback

The stand's HCL readback adapters match the immutable record ID and the
original `value` **and** the provider-persisted `operation_token`. Regate
derives the token from the durable effect ID **and the active HCL
readback/upstream contract**, rejects caller-supplied tokens, and injects it
into each actual write; the fixture fsyncs the token in the
same atomic file replacement as the record, including when its response is
deliberately lost. Reconciliation reconstructs the wire request from the
original operator input, journal ID and matching original contract and
requires the returned token to match. A changed active adapter fails before
any remote read, leaving the effect unknown. The process-restart test verifies token durability.

A negative test creates an **undispatched** journal entry targeting an
existing record whose ID **and payload are identical**, but whose operation
ID differs. Readback fails and the journal remains `unknown`, proving that
pure payload equality alone is insufficient. This controlled fixture is
not independently audited provider behavior, cryptographic provider
attestation, cross-process fencing or proof against a malicious/updatable
vendor server.


## Extended local durable provider contracts (PR #107)

Both **alpha and beta** expose the same additional *fixture-only* tools
directly on their loopback `/mcp` endpoints. They are intentionally **not**
added to the ordinary Regate public alpha/beta catalogs; `revoke_record` is
invoked only by authenticated, operator-approved
`/api/effect-compensate` or management MCP.

| Provider tool | Inputs | Durable invariant |
| --- | --- | --- |
| `put_record` | Existing fields plus optional `fence_epoch` | A second request with the *same nonempty provider-stored operation token*, exact ID, value and active epoch is acknowledged without a second commit; changed intent/reused token is rejected |
| `advance_fence` | `fence_epoch` (positive integer) | Epoch must strictly increase; fsynced epoch survives process restart; earlier generations are rejected on writes |
| `revoke_record` | `record_id`, optional `fence_epoch` and test fault | One compensating tombstone per original immutable token-bound record; original value, ID and operation token remain available for audit; duplicate inverse is rejected |
| `stats` | Empty | Adds `idempotent_replays`, `fence_epoch`, `fenced_rejections`, `compensations_received` and `compensations_committed` |

The standalone `advance_fence` helper is deliberately untrusted and
unauthenticated: it is allowed **only on the disposable loopback provider**.
A production implementation must authenticate the authority that promotes
epochs and bind effect calls to verified lease generations. A fixture can
prove a stale `fence_epoch` is rejected *after* observing a higher one; it
cannot make untrusted clients unable to choose an arbitrary higher number.

A compensation is not the same as deleting the record or reversing arbitrary
business effects. Both `alpha_undo` and `beta_undo` HCL contracts map a
previous `resolved_applied` write to a single `revoke_record` call against
the **same provider endpoint**. The effect journal records a new, separate
inverse with the operator approval, original source ID and the exact original
intent digest. The provider preserves a tombstone and original evidence
atomically; no duplicate inverse is dispatched.

**Important token binding:** the journal hashes the *wire* arguments after
Regate injects `operation_token`. During compensation the gateway
reconstructs the token using the still-active readback/upstream contract and
effect ID. The recomputed wire digest must match before any inverse call.
This prevents HCL contract drift or a caller-supplied token from approving
an inverse for a different write. Earlier tokenless compensations still use
their original arguments.

The race acceptance now runs **both**
`TestTwoProcessMCPStand` and
`TestTwoProcessMCPDurableProviderContracts`.
The second test verifies, for **both** real fixture processes:

- Lost post-commit MCP responses, `unknown` journal effects, exact
  provider-token readback and explicit `resolved_applied` verification.
- No unauthorized, cross-provider or duplicate inverse; durable tombstones
  and unchanged immutable original effects. Drifted active readback HCL or a
  caller-picked write token cannot authorize any inverse network call.
- A second inverse on **each** provider commits before reporting either MCP
  `isError` (alpha) or a lost TCP reply (beta). Regate keeps that inverse
  `unknown` without replay, a fresh provider read confirms the tombstone,
  and only then does an explicit audited operator decision mark it applied.
- Provider-native idempotent replay, altered-value/token collision rejection,
  fencing after epoch promotion, and persistence of tombstones, high-watermark
  epochs and counters after killing and restarting each provider.
- No automatic re-dispatch of `confirmed` or `unknown` Regate effect IDs.

The two independently persisted alpha and beta instances cover v1
durable-write acceptance. The tests establish the guarantees of the fixture
implementation. Guarantees for other providers depend on those providers'
contracts. [Acceptance decision](https://github.com/burkostya/regate/issues/106)
