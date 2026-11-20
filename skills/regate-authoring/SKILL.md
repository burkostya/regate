---
name: regate-authoring
description: Author curated MCP surfaces, virtual tools, DAG pipelines and external HTTP or MCP hooks in Regate.
---

# Regate authoring

Use Regate's management MCP interface. Inspect the upstream's actual `tools/list` schemas and current HCL before proposing virtual interfaces.

## Model

A surface maps an MCP-visible tool to either an upstream tool `source = "upstream.tool"` or a named `pipeline`. Pipelines have an entry node, named nodes and edges. A node produces an immutable value addressable as `nodes.<id>.value`; MCP results may also include `nodes.<id>.structured`.

### Pipeline node kinds

`input`, `output`, `transform`, `filter`, `validate`, `condition`, `parallel`, `join`, `map`, `pipeline.call`, `mask.pii`, `mcp.call`, `http.call`, `hook.call`, `policy.check`, `suspend`.

For `mcp.call`, specify `upstream`, `tool`, an `arguments` object containing expressions, and an explicit `effect = "read"` only if genuinely read-only. Unclassified operations are treated as potentially effectful. For `hook.call`, specify `hook` and an expression-valued `value`.

`condition` evaluates a boolean `test`. Edge `when` is `"true"`, `"false"`, or omitted for always. Parallelizable nodes execute concurrently when their dependencies are satisfied; `join` may wait for all, any, or a quorum. Avoid early joins around irreversible writes.

For bounded collection processing, use `kind = "map"` with `items`,
`max_items` (1..64), and **exactly one** of a pure CEL `mapping`
or a named `pipeline`. Pipeline-backed maps require object item
contracts and array result contracts. Explicitly set `allow_effects = true`
for a child that performs external writes. Regate prevalidates all input
items before dispatch and journals each iteration under a distinct
subrun ID; it does not automatically roll back a partially applied
batch or retry unknown external effects. For a mapped child that may
`suspend`, explicitly set `allow_suspend = true` as well and declare
the child's single terminal `output_schema`. The continuation path includes
the map node, its zero-based index as a string, and the nested suspend leaf
(e.g. `["create_many","0","review"]`). Regate saves the original items,
completed result prefix and active child checkpoint in SQLite; it never
restarts earlier iterations after an accepted answer. Every enclosing
call/map frame must opt into durable suspension.

Pipeline node expressions are compiled CEL programs with runtime cost limits and typed checking; static configuration uses HCL2/cty. `input`/`nodes` are dynamically typed at runtime, so verify external JSON Schemas and trust boundaries separately. See `docs/contracts/hcl.md`.

## Hook guidelines

- Decision hooks must return `{ "decision": "allow" }` or deny/indeterminate; do not treat a failure as approval.
- Transform hooks return `{ "action": "replace", "value": ... }`.
- External handlers receive JSON `{ "input": ... }`.
- Use the data owner's approved fields and recipients. Apply required local
  transformations before an external call and bind the call to their output.
  Treat external transform and decision hooks as recipients of their inputs.
- Configure timeouts. Carefully distinguish read-only and effectful actions.

## Authoring workflow

Inspect → draft → graph/HCL change → validate → effect/policy impact review → dry-run with mocks → operator review → activate → verify.

For reusable behavior, define a named pipeline instead of duplicating a long series of nodes. Prefer `regate_attribute_patch` for local attribute edits; `regate_graph_patch` preserves unrelated top-level blocks; for structurally unchanged graphs it also preserves nested comments, while structural graph edits may replace the whole target block. Avoid assuming the visual editor can losslessly rewrite every arbitrary HCL construct.
