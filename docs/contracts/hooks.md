# Hook contracts and lifecycle

## Goal

Hooks allow PII scrubbers, Decision APIs, external agent MCP tools, audit observers and transform services to participate in tool execution. The gateway remains authoritative for permissions; the hook response never grants a caller more rights than configured in the gateway.

## Hook transports

1. `builtin`: local Go transformer; the prototype includes simple email/key masking, **not robust PII declassification**.
2. `http`: HTTP POST JSON `{ "input": value }`, optional `auth_env`, request timeout and JSON response.
3. `mcp`: `tools/call` on a configured upstream MCP tool, with JSON object arguments.

### Decision result

```json
{"decision":"allow","obligations":[]}
```

Any response other than explicit `allow`, including network timeout or `indeterminate`, is denied. If `obligations` is nonempty, the prototype fails closed because it cannot prove the obligations were enforced.

### Transform result

```json
{"action":"replace","value":{"customer_id":"X", "email":"[REDACTED]"}}
```

The runtime returns the `value`. A hook returning `{ "action":"deny" }` aborts. Other control actions such as `route`/`retry`/`suspend` are **not** part of the implemented hook contract despite being planned for complete v1.

## Interceptor lifecycle

- `on_request`: invoked on public tool arguments before routing.
- `before_call`: invoked before outbound MCP or HTTP call; for MCP tool calls the result must be an object.
- `after_call`: transforms the external result; if changed, the raw MCP response must not be copied unchanged to the client.
- `before_response`: transforms the final value.
- `on_error`: best-effort observer on an error; not allowed to suppress or replace the original error.

Interceptors are sorted by `order` and then `name`, optionally scoped to surface and tool. A missing or failing interceptor fails closed (except the observer hook, which cannot replace the original failure). The UI should always show their placement relative to explicit graph nodes.

## Security considerations

- Place the trusted mandatory policy check before reading or transmitting protected data.
- HTTP and MCP hooks are external transfers. They need independent trust, authentication and effect contracts.
- Avoid logging request or response values that could contain PII. Trace journal stores statuses and messages rather than full values by default, but error text can still contain sensitive data.
- Fail-closed is necessary but not sufficient: trustworthy classification, audit and authorization enforcement are required before production.
- The PII masker should return a verifiable declassification result only after a trusted, testable procedure. The current prototype does not remove data labels or implement information-flow proofs.


## Resuming a suspended pipeline (PR #59)

The final public response boundary is shared by the initial `Run` and
durable `Resume` paths. A still-suspended execution does **not** run
`before_response`. When a resumed root completes, Regate enforces
root-output exclusivity, validates the public tool's `output_schema`,
runs the ordered `before_response` interceptors, and validates the
transformed result again. A changed result clears an earlier MCP envelope;
failed validation or a failed hook clears all public result fields and
marks the run failed.

Once the graph completes, the consumed durable checkpoint is deleted even
when its final response schema or response hook fails. It cannot be
resumed again to replay an already dispatched external HTTP/MCP effect;
unknown hook outcomes instead remain in the effect journal for explicit
reconciliation. Failures are traced with redacted diagnostics and the
best-effort `on_error` observer. This does **not** mean an external
effect has been rolled back, and a partially completed *graph* that
fails before reaching the final response boundary still follows its
separate effect reconciliation policy.
