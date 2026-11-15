# Regate release acceptance / implementation status

**Target scope (user-approved):** full first release includes HTTP-only MCP client/server, editable virtual tools/resources/prompts, HCL2 declarative config, typed transformations/CEL, DAG branches and compositions, HTTP/MCP hooks including agents/PII/Decision API, policies, revisioned management MCP and skills, SvelteKit+Svelte Flow visual editor, async tasks, diagnostics, security and tests. **Do not silently split or drop these features.**

**Current deliverable:** a self-contained dependency-free **Go engineering prototype** plus frontend **source**, NOT complete v1. This distinction is important when planning installation or deployment.

## Implemented and locally verified

| Area | Status | Evidence |
|---|---|---|
| Go CLI + local HTTP server | Implemented | `go build`, smoke test |
| Upstream MCP HTTP discovery and tool calls | Partial implementation | Mock HTTP MCP integration tests |
| Public tools with direct source and DAG pipeline | Implemented subset | `TestEndToEndMCP`, `TestMCPDirectProxyPreservesContent` |
| HTTP Decision API / PII transform / MCP agent hook | Implemented subset | `TestEndToEndMCP`, deny test |
| `on_request`, `before_call`, `after_call`, `before_response`, `on_error` hooks | Partial implementation | Go code and integration paths |
| HCL parsing/validation and safe expressions | Restricted implementation | config + expr unit tests |
| DAG conditional edges/parallel waves/join | Basic implementation | Code path; incomplete formal/race coverage |
| Prompt/resource publishing and proxy resource read | Partial implementation | Server method path |
| Management MCP and REST endpoints | Implemented subset | Auth + draft/activate/conflict/rollback tests |
| Immutable revision history + drafts | File-backed implementation | Integration tests |
| Dry-run with explicit mocks | Partial implementation | REST API path |
| Effect journal (`dispatching`, `confirmed`, `unknown`) | Partial implementation | Go executor |
| Async MCP tasks, cancellation, checkpoint and resume | Partial implementation | Task integration test |
| Subscription change notification | Implemented subset | SSE activation test |
| Outbound HTTP DNS/IP safety guard | Implemented initial fail-closed guard | `internal/netguard` unit tests (DNS rebinding / mixed DNS / loopback) |
| Admin skills + `skills/list`, `skills/get` / `skill://` resources | Implemented subset | Admin skill test |
| SvelteKit + Svelte Flow editor | **Authored but unverified** | No npm install or build possible in offline environment |
| Go race tests, vet, build | **Passed** | `go test -race ./...`, `go vet ./...`, `go build ...` |

## Unfinished functionality required for full v1

1. Replace restricted parser and evaluator with HCL2, `cty` and CEL, including interpolation support and complete port contract typing.
2. Replace hand-written MCP wire subset with official `modelcontextprotocol/go-sdk`; cover protocol conformance, OAuth, all supported transports/versions within HTTP, task/skill extensions and subscription semantics.
3. Full JSON Schema input/output validation, compatibility checks and source-map/opaque-safe bidirectional editing of arbitrary HCL2 fragments.
4. Full composition semantics (`pipeline.call`, map, controlled retries, named ports, safe joins with effects), robust task and MRTR resume, and property-based execution tests.
5. Production-grade effect reconciliation/idempotency/compensation, backpressure and durable scheduling (no blind retries after uncertain writes).
6. Enforced data labels/taint tracking and verified PII obligations, permissions, principal identities and trust boundaries throughout every node.
7. Persist revisions/events/traces/tasks in SQLite with transactions; safe migrations and recovery/compaction.
8. OAuth and scoped permissions for admin, user/surface, and upstreams; production-audited egress/SSRF rules with explicit internal-service configuration; secure secret handling.
9. Real independent MCP resource/prompt catalog discovery and edit/mapping; upstream change detection and compatibility diagnostics.
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
