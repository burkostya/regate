# Regate v0.1 configuration syntax (restricted HCL)

## Scope and warning

The current parser (`internal/config/hcl.go`) accepts a **constrained HCL-like grammar**, not full HashiCorp HCL2. Supported: quoted block labels; attributes assigned strings, integers, floats, booleans, null, maps/lists; comments `#`, `//`, `/* */` (as implemented); child blocks. Unsupported: full HCL expressions, interpolation, `file()`, `for` comprehensions, dynamic blocks, modules, unquoted references. A malformed or unsupported expression is rejected rather than executed.

The aim is to replace this adapter with `hashicorp/hcl/v2` + `cty` and a CEL or equivalent typed expression compiler after dependencies are available. Do **not** claim interchangeability with Terraform HCL or CEL today.

### Top-level blocks

- `regate { listen = "127.0.0.1:8080" }`
- `upstream "name" { url = "https://example.org/mcp" auth_env = "TOKEN_ENV" timeout_ms = 10000 }`
- `hook "name" { transport = "http|mcp|builtin" ... contract = "transform|decision|observe|action" on_error = "deny" }`
- `policy "name" { decision_hook = "hookName" }`
- `interceptor "name" { stage = "on_request|before_call|after_call|before_response|on_error" hook = "hookName" surface = "optional" tool = "optional" order = 100 }`
- `pipeline "name" { entry = "first" timeout_ms = 60000 concurrency = 8 node "x" { ... } edge { from = "x" to = "y" } }`
- `surface "name" { path = "/mcp/name" tool "public" { ... } resource "..." { ... } prompt "..." { ... } }`

### Expressions (not CEL)

In node attribute positions such as `value`, `arguments`, `mapping` and `test`, a **quoted string** is parsed as a safe Go-expression subset, e.g. `"input.customer_id"`, `"nodes.fetch.value"`, `"input.count > 3"`, `"lower(input.email)"`. Maps and lists evaluate their string leaves recursively; use `literal:hello` for a literal string. The expression engine has no file access, arbitrary call/exec, unbounded recursion or network support. It is not a general Go interpreter.

Example:

```hcl
node "fetch" {
  kind      = "mcp.call"
  upstream  = "crm"
  tool      = "get_customer"
  effect    = "read"
  arguments = { customer_id = "input.customer_id" }
  mock      = { customer_id = "literal:C123" }
}
```

`effect = "read"` must be used only for genuinely read-only operations. `unknown`/omitted effects are journaled as possibly modifying an external resource.

### Pipeline node kinds

| Kind | Important attributes | Behavior in prototype |
|---|---|---|
| `input` | — | Emits tool arguments |
| `output` | `value` | Selects tool return value |
| `transform` | `value` or `mapping` | Computes value |
| `filter` | `value`, `fields` | Copies selected fields |
| `validate` | `value`, `required` | Checks object and required fields only |
| `condition` | `test` | Boolean; edges can use `when = "true"/"false"` |
| `parallel` | `value` | Pass-through/control marker, not nested subpipeline |
| `join` | `mode = "all|any|quorum"`, `quorum`, `value` | Waits for all/any/quorum predecessor states |
| `mcp.call` | `upstream`, `tool`, `arguments`, `effect`, `mock` | Remote MCP call and effect journal |
| `hook.call` | `hook`, `value` | HTTP/MCP/builtin processing |
| `http.call` | `url`, `value`, `auth_env`, `timeout_ms` | HTTP transformer |
| `policy.check` | `policy`, `value` | Explicit decision hook |
| `mask.pii` | `value` | Simple regex/key-based masking, NOT strong de-identification |
| `suspend` | — | Store checkpoint; await separate resume |

### Surface tools

Each `tool` specifies exactly one of:

- `source = "upstream.remote_tool"`: direct transparent MCP proxy (no pipeline transformation).
- `pipeline = "named_pipeline"`: use the execution graph.

Use `input_schema` / `output_schema` as literal object maps. These schemas are published in `tools/list`; **full JSON Schema validation is not yet implemented**.

### Revision workflow

`GET /api/config` or admin MCP `regate_config_get` → `regate_draft_create` → `regate_draft_patch` or `regate_graph_patch` → `regate_validate` → `regate_diff` and `regate_impact_analyze` → `regate_test` → `regate_activate`.

`graph_patch` replaces exactly one top-level pipeline source span and preserves bytes outside it; it does not preserve every comment within the replaced pipeline. Do not mistake this for a fully lossless HCL AST editor.

### Known limitations

There is no complete type-checker across ports, no formal subpipeline-call primitive, incomplete expression operation coverage, no dynamic discovery of upstream schema changes at activation, and no source-map-preserving HCL2 formatter. These belong to the complete v1 acceptance contract.
