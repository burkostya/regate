# Regate

**Regate** is an experimental, HTTP-only **MCP gateway and virtual-tool composition engine** written in Go, with a declarative HCL2 configuration, programmable HTTP/MCP hooks, a management MCP endpoint and a SvelteKit + Svelte Flow console.

**Release status.** The full v1 functional scope passed local acceptance on
the controlled alpha/beta stand. The
[v1.0.0-rc.1 prerelease](https://github.com/burkostya/regate/releases/tag/v1.0.0-rc.1)
contains the tested archive and report.
Operational security and privacy approval are pending in
[#113](https://github.com/burkostya/regate/issues/113).
[Release status and evidence](docs/release-status.md)

## What you can experiment with now

- Connect HTTP MCP upstream servers; expose renamed/curated tools through independent `surface` endpoints.
- Expose tools from a DAG of transformations, policy decision hooks, PII masking, HTTP calls and other MCP tools.
- Discover upstream tools, proxy existing MCP tool results, publish local resources and prompts.
- Inspect, draft, validate, diff, activate and roll back configuration through a **separate authenticated management MCP** and matching REST API.
- Use stateless MCP `2026-07-28` discovery/tools/tasks/skills requests and local `subscriptions/listen` change notifications; legacy `initialize` has basic compatibility.
- Run dry-run scenarios with mocks, inspect durable SQLite traces and effect journal; use task IDs, encrypted bounded queues and suspended-run checkpoints.
- View/edit a graph and its HCL in an authenticated SvelteKit/Svelte Flow console, tested in Chromium against a real gateway and served by a production Node adapter (requires npm dependencies).

## Requirements

Backend: Go **1.25+**, using pinned HCL2, cty and CEL modules (see `go.mod`/`go.sum`). Frontend: Node 22+, npm and connectivity to npm for its dependencies. Real MCP adapters require compliant upstream servers; the demo includes local fixtures.

## Verify a SQLite backup offline

A Regate SQLite backup should be created through the server's
`storage.Store.Backup` / `BackupVerified` API (WAL-consistent
`VACUUM INTO`, verified and atomically published). To verify an existing
snapshot without starting the server or requiring `REGATE_ADMIN_TOKEN`:

```sh
go run ./cmd/regate -verify-backup /path/to/regate.sqlite
```

The output is a non-secret JSON manifest including schema, current
revision, record counts, file size and SHA-256. It does not include
HCL source or encrypted task input. Save any required grant/queue
encryption keys in separate protected key escrow; SQLite structural
verification does not validate those external secrets or prove any
remote resource state. A snapshot also includes the execution lease of
its source; do not start a restored worker against the same upstream
services until the original worker is stopped/fenced and the copied
lease has expired.

## Run a complete local demonstration

Terminal A, from repository root:

```sh
go run ./cmd/regate-demo
```

Terminal B:

```sh
export REGATE_ADMIN_TOKEN='replace-this-with-a-long-secret'
go run ./cmd/regate -config ./examples/regate.hcl -state ./.state
```

Try the virtual tool with the legacy-compatible JSON-RPC form (from the same machine):

```sh
curl -sS http://127.0.0.1:8080/mcp/support \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"summarize_customer","arguments":{"customer_id":"C123"}}}'
```

Expected result: a summary from the demo agent containing masked PII, e.g. `Contact [REDACTED]`. Try `customer_id="DENY-1"` to exercise the policy denial. This is only a demonstration: `get_customer_direct` intentionally bypasses the masking pipeline to show transparent tool proxying, so do not expose the demo surface to other users.

Use the protected management endpoint:

```sh
curl -sS http://127.0.0.1:8080/mcp/admin \
  -H "Authorization: Bearer $REGATE_ADMIN_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"regate_status","arguments":{}}}'
```

Admin resources include `skill://regate-config/SKILL.md` and `skill://regate-authoring/SKILL.md`. The gateway reads these files from the directory named by `REGATE_SKILLS_DIR` (default `./skills` from repository root).

### MCP revision 2026-07-28

For the latest stateless request shape, set HTTP header `Mcp-Protocol-Version: 2026-07-28` and include:

```json
"_meta": {
  "io.modelcontextprotocol/protocolVersion": "2026-07-28",
  "io.modelcontextprotocol/clientCapabilities": {}
}
```

inside every `params` object except `server/discover`. The local prototype does **not** guarantee compliance with the entire protocol revision; the official SDK is the intended final implementation.

## Open the visual console

From the repository root, while Regate is running:

```sh
cd web
npm install
npm run dev
```

Open `http://127.0.0.1:5173`, enter `REGATE_ADMIN_TOKEN`. The same-origin SvelteKit server proxy sends `/api/*` to `http://127.0.0.1:8080` in both development and built Node deployments; override its **server-side** `REGATE_PROXY` environment variable if Regate runs elsewhere. It forwards the admin bearer only to that configured backend, refuses backend redirects and does not cache responses. The token lives only in memory in the browser tab.

To exercise the **built** console locally, still keeping it bound to loopback:

```sh
cd web
npm run build
REGATE_PROXY=http://127.0.0.1:8080 HOST=127.0.0.1 PORT=5173 node build
```

**Frontend CI.** Checks include npm type checks, graph guard and unit tests,
a production Node build and Chromium E2E against the Go gateway and
disposable SQLite state.

## Optional container image (for controlled local experiments)

```sh
podman build -t regate:prototype .
podman run --rm --network host -e REGATE_ADMIN_TOKEN='replace-this' \
  -e REGATE_PUBLIC_TOKEN='replace-this-public-token' \
  -v "$PWD/.state-container:/data:Z" regate:prototype
```

**Important:** the shipped sample upstream URL is `127.0.0.1:9091`, so it works with host networking on Linux, not a default bridged container network. Adapt upstream URLs and configure HTTPS/TLS/authorization before any remotely accessible deployment. The container starts listening on all interfaces and requires `REGATE_PUBLIC_TOKEN`. The local demo is not a deployment template.

## Quality checks

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/regate ./cmd/regate-demo
```

The Go tests cover parser and expression safety, HTTP MCP end-to-end flow, decision denial, data redaction, management authentication/revisions/conflicts, original-content proxying, subscriptions, tasks and skills. They do not replace official MCP conformance tests, threat modeling, load tests or deployment testing.

## Configuration contract

See [HCL2/CEL compiler contract](docs/contracts/hcl.md) and [architecture](docs/architecture/overview.md). Example: [examples/regate.hcl](examples/regate.hcl).

Configuration has two representations:

1. **Source**: HCL2 text, saved as immutable revisions and editable drafts. Initial `-config` file bootstraps a *new* state directory; later restarts use persisted active revision.
2. **Layout**: separate JSON node coordinates for Svelte Flow. It never participates in execution.

The Go runtime evaluates DAGs. A node can return a JSON value or an MCP result payload. Interceptors are scoped by surface/tool and configured in HCL. Explicit `on_request`, `before_call`, `after_call`, `before_response`, `on_error` stages are supported for selected runtime paths. `on_error` is observational and cannot override a denial.

## Security warnings

**Do not expose this prototype to the public internet or use real PII/secrets.** In particular: OAuth, trustworthy principal propagation, full data-label tracking and declassification, verified upstream effects/reconciliation, hardened SSRF/DNS rebinding defenses, resource budget limits, complete JSON Schema enforcement and a security-reviewed MCP transport are **not implemented**. The built-in email redactor is a demo utility, **not** a sufficient PII scrubber. The `get_customer_direct` demo tool intentionally exposes raw data.

Required bootstrap controls:

- `REGATE_ADMIN_TOKEN` — mandatory for the CLI and for management tools.
- `REGATE_PUBLIC_TOKEN` — mandatory if listening on a non-loopback interface.
- `REGATE_ALLOWED_HOSTS` — comma-separated allowlist for outbound HTTPS hosts; localhost is permitted for demos. Network redirects are disabled.
- `REGATE_SKILLS_DIR` — absolute path to skill files in deployed installations.
- `auth_env` in HCL — *name of an environment variable*, not a bearer token value.

See [security notes](docs/security.md) and [release status](docs/release-status.md).

## Project layout

- `cmd/regate` — gateway CLI; `cmd/regate-demo` — local sample upstream and HTTP hooks.
- `internal/config` — HCL2/cty parsing, strict validation, typed IR, semantic diff and source-preserving patches.
- `internal/expr` — bounded evaluator, demo masking function.
- `internal/engine` — DAG execution, MCP/HTTP actions and effect journal calls.
- `internal/mcp` — experimental HTTP MCP client.
- `internal/server` — MCP endpoints, management REST, subscriptions, tasks, skills.
- `internal/storage` — durable file-backed revisions/drafts/journal/tasks/checkpoints.
- `web` — SvelteKit/Svelte Flow console source.
- `skills` — agent operational instructions, also published by management MCP.
- `docs` — architectural contracts, decision records, formal sketch and remaining work.

## License

No license has been selected yet. The source is provided for review and development; select a license before public redistribution.

### Browser end-to-end acceptance test

The Web workflow also provides an opt-in Chromium E2E suite against a
real Go gateway, the local demo MCP upstream and SvelteKit. Run all
three processes using the local demonstration commands above, then:

```sh
cd web
npm install
npx playwright install chromium
REGATE_E2E_ADMIN_TOKEN="$REGATE_ADMIN_TOKEN" npm run test:e2e
```

By default the E2E test creates and edits a disposable draft but
**does not activate or roll back**. Activation/rollback checks are enabled
only with `REGATE_E2E_ALLOW_ACTIVATION=true`. **Never enable that flag
against production or shared administrator state.** GitHub Actions opts
in only after provisioning an isolated temporary SQLite state directory,
binding Regate to `127.0.0.1`, starting the demo MCP provider, and
verifying process readiness. Failure screenshots and traces are retained.

### Recover an outdated console draft

If the console reports `draft source conflict`, another writer updated
the same draft after you loaded it. In the HCL tab choose **Reload draft**
to fetch the current persisted source and SHA-256, review the other
writer's changes, then save your next edit. If you have unsaved local
HCL or graph changes, you must explicitly confirm their discard; cancel
to preserve them. Reload never performs an automatic three-way merge,
and a draft with a stale base revision cannot be activated as-is.

### Administrative JSON request safety

Administrative REST endpoints accept a single JSON object, with a
maximum request body size of 2 MiB. Trailing second JSON documents,
non-whitespace junk, duplicate keys (including nested/escaped
equivalents) and nesting deeper than 128 are rejected before executing
an administrative operation. This parser is independent from the
official SDK's MCP transport and intentionally does not alter the
HCL source bytes carried in a valid `source` string.

### Safe two-process MCP acceptance stand

Two independent loopback HTTP MCP providers (alpha and beta) use the
official Go SDK and separate durable synthetic record stores. The
integration test covers read/write isolation, a policy-gated cross-provider
pipeline, lost responses after committed writes, and positive readback.

```sh
make test-mcp-stand
```

For manual startup and recovery scenarios, see
[the two-server stand](docs/testing/two-mcp-stand.md).
