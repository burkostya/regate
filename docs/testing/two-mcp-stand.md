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
5. Killing alpha, restarting it from the same state directory and reading
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
| `put_record` | `record_id`, `value`, optional `fault` | Create immutable test record (never overwrite) |
| `get_record` | `record_id` | Read record; not found is not authoritative absence |
| `stats` | empty object | Read received writes, committed writes, record and read counters |

Record IDs are limited to 48 safe ASCII characters; values to 256 UTF-8
bytes. There are at most 128 records per process. The special fault value
`drop_after_commit` is accepted only on a `put_record` request.

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
`"fault":"drop_after_commit"`. The client must see an error but a read
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

The stand's HCL readback adapters match both an immutable record ID and the
exact original test `value` against the observed provider record. The optional
`match_intent_pointer` and `match_result_pointer` pair is validated at
compile time and checked after the usual endpoint/intent-digest/identity
barriers. The negative test inserts a synthetic undelivered journal attempt
whose ID already exists with a **different payload**, then proves that
readback cannot falsely resolve it as applied. Equality is not equivalent
to provider consistency/fencing attestation and cannot prove a write when
some other actor may have produced an identical record.
