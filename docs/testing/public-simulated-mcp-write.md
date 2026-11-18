# Third-party simulated MCP write acceptance (no account)

**Provider:** Sondahub — https://www.sondahub.com/mcp-server/
**Remote Streamable HTTP:** `https://api.sondahub.com/mcp`
**Vendor policy:** anonymous test calls, 40 requests/minute per IP as of
November 2026; write operations are **simulated**, not committed to shared
persistent state. The provider returns a session token in
`X-Sondahub-Session` which a client can explicitly carry to observe a
session-scoped synthetic write. This Regate acceptance does **not** carry
that token, verify cross-request persistence, or assert provider-native
idempotency, fencing or compensation.

## Explicit safety boundary

`examples/public-simulated-write-mcp.hcl` is a dedicated **test-only**
configuration. It binds Regate to `127.0.0.1`, permits egress only with
`REGATE_ALLOWED_HOSTS=api.sondahub.com`, and publishes exactly two
unprivileged test commands behind one local surface:
- `create_fixture`: one synthetic, vendor-simulated create_record in
  `store.categories`, with all arguments **literal and pinned in HCL**.
  The published output only confirms the presence of the vendor's documented
  `_note` marker. It does not expose the created record or an HTTP session
  token.
- `reject_fixture`: intentionally omits the required `slug` field of
  `store.categories` to exercise a live MCP `isError=true` response.
  No real entity is changed, according to the vendor's documented sandbox
  semantics. Regate nonetheless journals this as `unknown`, because a
  generic remote provider may commit before reporting an application error.

The surface declares both tools **write-capable** (`read_only = false`).
Its tool input JSON Schema forbids extra properties and the pipeline never
uses inbound values in the provider's synthetic record. Neither public
customer data nor secrets nor live user-provided write targets are forwarded.
Only fixed synthetic values are passed to the external provider.

## Running

Normal `go test ./...` runs a deterministic HCL compiler test and skips
this external write test. To enable the real-network checks explicitly:

```sh
REGATE_TEST_PUBLIC_MCP_WRITES=1 go test -race ./internal/server \
  -run '^TestPublicSimulatedMCPWriteAndError$' -count=1 -timeout 3m -v
```

The separate `.github/workflows/public-simulated-mcp-write.yml`
executes this on relevant changes, manual dispatch and weekly. Do not
enable test writes against any different MCP URL without a new explicit
fixture and a provider-specific contract review. Vendor outages, rate limits,
protocol changes and any change from simulated to persistent writes must
be treated as an external acceptance failure, not a reason to silently
substitute mocks or automatically retry writes.

## What it verifies — and does not

An independent third-party MCP implementation really receives a Regate
effectful `tools/call` after a SQLite write admission barrier. Its valid
response produces a `confirmed` effect record with immutable intent and
target digests. Its MCP application error produces `unknown`; trying to
admit the same logical run/node/operation again returns
`ErrEffectAlreadyAttempted` without network re-dispatch. Invalid inbound
arguments cannot override the pinned synthetic HCL record.

This does **not** demonstrate crash consistency of the provider, recovery
of actual persisted external data, lost-response readback, provider-enforced
idempotency tokens, verified compensation, durable retention, transaction
coordination, an approved production writable sandbox, or v1 release
readiness. Those still need an independently operated external writable
system that durably retains records and binds identity/fencing evidence.
Keep the local alpha/beta two-process MCP stand as the reproducible
write/restart/fault-injection oracle, and Microsoft Learn/DeepWiki as the
real read-only provider acceptance.

Provider references:
- https://www.sondahub.com/mcp-server/ (protocol, simulated create/error
  tools, session header and fair-use limit)
- https://www.sondahub.com/apis/store/ (categories schema with required
  `name` and `slug`)
