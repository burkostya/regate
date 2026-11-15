# Agent instructions for Regate

Regate is an HTTP-only MCP gateway. The user-approved **v1 scope is not a reduced MVP**: it includes HCL2 and CEL, full MCP compatibility, typed DAG execution, composable HTTP/MCP hooks, Decision API and PII controls, effect reconciliation, SQLite durability, management MCP and skills, and a SvelteKit + Svelte Flow visual editor.

## Development protocol

1. Read `docs/release-status.md` before claiming a component is complete. This imported prototype is not the finished release.
2. Implement changes on feature branches. Prefer focused commits and PRs; do not force-push shared branches.
3. Preserve HTTP-only transport. Do not introduce stdio MCP connections.
4. HCL is the executable source of truth; UI layout is a separate JSON projection. Never silently discard opaque HCL fragments.
5. Treat upstream MCP responses, HTTP hooks, agent outputs and MCP annotations as **untrusted**. Never put credentials or raw sensitive payloads into logs, traces or config.
6. Protect management endpoints. No auth bypass, permissive default or write retry on unknown external effect outcomes.
7. Run `go fmt ./...`, `go vet ./...`, `go test -race ./...`, `go build ./cmd/regate ./cmd/regate-demo` before each PR. For web changes, run `npm run check` and `npm run build` inside `web/` when dependencies are available. State any unrun checks explicitly.
8. Update docs and tests with every contract change. An incomplete implementation must be marked as such, not labeled production-ready.

## Key locations

- `internal/config`: HCL2/cty parser, closed schemas and typed IR migration
- `internal/engine`: pipeline runtime and hooks
- `internal/server`: MCP and management HTTP handling
- `internal/storage`: current file-backed store (replace with SQLite)
- `web`: SvelteKit + Svelte Flow frontend (build not yet verified)
- `skills`: administrator and authoring agent skills
- `docs/release-status.md`: outstanding release gates and acceptance scenarios
