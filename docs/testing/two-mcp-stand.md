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

**This replaces the external durable sandbox as the current *local
engineering acceptance*, not as third-party provider attestation.** Real
independent durable writable MCP provider verification is deferred to
[issue #106](https://github.com/burkostya/regate/issues/106). Never claim
that two local processes with one fixture implementation establish a
production exactly-once guarantee or verify independent vendor behavior.
