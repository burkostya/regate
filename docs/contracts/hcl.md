# Regate HCL2 and CEL compilation contract

HCL is the sole executable source. UI coordinates/layout remain separate JSON.
Regate uses `hashicorp/hcl/v2/hclsyntax` for parsing and `cty` for static
configuration values. All executable HCL blocks have a closed schema: unknown
attributes, child blocks and wrong types prevent activation. Native HCL2
arithmetic, conditionals, maps, lists and for expressions can be used for
**static** configuration values. Variable references and calls to arbitrary
functions are intentionally not available in HCL configuration evaluation.

Node attributes `value`, `mapping`, `arguments` and `test` are different:
strings in these positions are **CEL programs**, not HCL expressions. CEL
expressions use dynamically typed `input` and `nodes` activation variables
because remote MCP schemas may vary at runtime. Unknown variable names and
statically invalid operations are rejected during configuration compilation.
The `condition.test` expression must return a Boolean (or a dynamic value
whose Boolean type is verified at runtime). Nested maps/lists compile their
string leaves recursively. Prefix a string with `literal:` to opt out of CEL
and return it as an ordinary string. Examples:

```hcl
pipeline "customer_summary" {
  entry       = "fetch"
  timeout_ms  = 2000 * 30
  concurrency = 4

  node "fetch" {
    kind      = "mcp.call"
    upstream  = "crm"
    tool      = "get_customer"
    effect    = "read"
    arguments = {customer_id = "input.customer_id"}
    mock      = {customer_id = "C123"}
  }

  node "finish" {
    kind  = "output"
    value = "nodes.fetch.value"
  }

  edge {
    from = "fetch"
    to   = "finish"
  }
}
```

### Compilation before activation

`Decode → HCL2 parse → cty value evaluation → closed schema validation →
reference/graph validation → CEL compilation → deterministic typed IR`.
The IR contains a topological node order, output port categories, source byte
ranges, compiled CEL programs and validated static `nodes.NAME` dependencies.
An expression referencing a missing node or a node that cannot precede it is
rejected at activation. A dynamic map index, conditional path, or untrusted
remote response can still fail at runtime; **this is not a proof of full
JSON Schema compatibility**.

CEL uses `cel.CostLimit(10000)` for each evaluation, with an 8192-byte
expression source cap. No filesystem, network, subprocess or arbitrary Go
function is exposed to CEL. The legacy expression helpers `lower`, `upper`,
`contains`, `str` and `redact` are compatibility functions; new configs should
prefer standard CEL methods where possible.

### Supported blocks and node kinds

Top-level blocks: `regate`, `upstream`, `hook`, `policy`, `pipeline`,
`surface`, `interceptor`. Within pipelines: `node` and `edge`. Within
surfaces: `tool`, `resource` and `prompt`.

Node kinds: `input`, `output`, `transform`, `filter`, `validate`, `condition`,
`parallel`, `join`, `mcp.call`, `hook.call`, `http.call`, `policy.check`,
`mask.pii`, `suspend`. All node attributes are validated by kind; unknown
attributes fail closed. An `mcp.call` must declare its upstream, tool and
optional effect. `read` must only be asserted for genuinely read-only calls.
The default `unknown` effect is journaled as a potential write. Current hook
failure handling is `deny` only (no fail-open policy).

A surface tool specifies exactly one of `source = "upstream.tool"` (direct
proxy) or `pipeline = "named_pipeline"`. `input_schema` and `output_schema`
are native HCL object values published with the tool. Full JSON Schema
validation of remote content is a separate v1 requirement.

### Source-preserving edits and semantic diffs

`regate_graph_patch` replaces one explicitly named pipeline block without
changing any bytes outside it. When the original and replacement blocks
have the same nested structure and attribute keys, it patches **only changed
attribute expressions**, preserving operator comments and unsupported text
inside the pipeline as well. `regate_attribute_patch` (and REST
`POST /api/attribute-patch`) replaces **only an existing attribute expression**
in a draft. The required `path` is a slash-separated sequence of block type
and label, e.g. `pipeline/customer_summary/node/fetch`, plus `attribute` and
`expression` (new HCL2 source). It refuses ambiguous paths, missing attributes,
syntax errors and any change that makes the whole configuration invalid.

The source retains comments, whitespace and opaque content outside the edit.
A **structural** graph replacement (adding/removing/reordering nodes, edges
or attributes) falls back to replacing the entire named block: comments
inside that block are not guaranteed to survive. Introducing new attributes
currently also requires block replacement. These limits are visible and
remain in scope for full v1 acceptance.

`regate_diff` and `regate_impact_analyze` provide structural summaries and
`semantic_changes` with stable paths for added/removed/changed executable
values. Whitespace, source spans and comments alone are not semantic changes.

### Remaining limitations

The compiled IR is typed at the known HCL and CEL expression boundary, not at
all dynamic MCP port boundaries. Pending: full cross-port JSON Schema typing,
verified remote contract resolution, explicit pure data dependency edges,
source-preserving *insertion/removal* of nested blocks and attributes, and
arbitrary opaque block editing in the visual editor. Dynamic CEL subscripts
and paths through conditional branches need runtime validation.

### Declared JSON Schema contracts (compiler extension)

Surface tools use `input_schema` and `output_schema`. Every pipeline `node`
may also declare `input_schema` and `output_schema` as inline HCL objects. All
schemas are compiled with JSON Schema Draft 2020-12 before the revision can be
activated. Local `$defs`/`$ref` are supported; remote schema downloads are
forbidden during compilation. Unknown external schemas cannot silently
introduce new network dependencies.

The `input_schema` of an `mcp.call` node constrains the effective `arguments`
object immediately before dispatch, *after* `before_call` interceptors. The
input schema of `hook.call` and `http.call` similarly constrains the value sent
outside. A declared node `output_schema` constrains the resulting value before
it enters another node. Surface input/output schemas are enforced at the MCP
boundary, both before and after interceptors.

The compiler rejects obvious incompatibilities (disjoint JSON types and
required properties that a closed producer can never provide), but does not
claim that arbitrary JSON Schema subsumption is decidable. Dynamic/unproven
links are checked against the consumer schema at runtime. This is essential
for untrusted remote MCP payloads and conditional CEL expressions. A running
revision retains its compiled schemas.

### Structural HCL edits

`regate_graph_patch` performs a source-range merge rather than replacing the
entire pipeline. It matches named blocks and `edge` blocks by endpoints and
condition, updates only changed expressions, and handles additions/removals
without rewriting siblings. Edits are rejected if they target ambiguous block
identities or produce an invalid executable configuration. Comments immediately
outside changed ranges are preserved. Deleted elements cannot preserve content
*inside* their own deleted range; for this reason a visual editor should warn
when deleting a node with hand-written documentation. `AddAttribute` provides
a targeted insertion operation in the underlying configuration package.
