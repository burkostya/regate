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

### Global active-task admission

The optional top-level `regate { max_active_tasks = N }` setting caps the
number of asynchronous tasks that may run concurrently in one SQLite state
directory. Default: **32**, accepted range: **1–1024**. This is a
system-wide bound across public surfaces and individual pipelines; it is
independent of the existing per-pipeline `concurrency` limit for nodes.

A task start or user-approved continuation reserves an active slot through
an atomic SQLite transaction *before* its external side effects or goroutine
execution. Capacity exhaustion rejects new work rather than accepting an
unbounded background queue. Suspended tasks (`input_required`) do not
occupy active slots; completion, cancellation and failure release slots.
A grant-backed continuation reserves its slot in the same transaction as
its OAuth-grant refresh barrier. It cannot bypass the cap.

This is an explicit **backpressure milestone**. A durable encrypted
waiting queue, admission fairness and priority scheduling are still separate
release requirements; this version deliberately rejects overflow rather
than persisting unbounded, potentially sensitive task arguments.

### Bounded encrypted waiting queue declaration

In addition to `max_active_tasks`, the top-level
`regate { max_queued_tasks = N }` attribute defines an optional, bounded
SQLite waiting queue (default 0, maximum 4096). A queue requires a separate
`REGATE_QUEUE_KEYS_JSON` keyring with 32-byte AES-256-GCM keys. Without keys
the system must not persist queued arguments in plaintext.

Deferred execution supports non-OAuth tasks without caller-delegated
credentials or required scopes. An expired bearer cannot be reused to
execute sensitive queued work. A queue entry stores original inputs
encrypted, bound to task owner/revision, original trust requirements and
TTL. The live dispatcher claims work in FIFO order **within each owner**
and alternates owners by least-recently-served turn whenever an active task
slot becomes available, including after restart. A claimed task interrupted
before completion is failed closed rather than automatically replayed.
Queued inputs must have a positive TTL of at most 24 hours. The
dispatcher authenticates and removes expired ciphertext even while
all execution slots are occupied, without replaying any external action.

`regate.max_queued_per_owner = N` limits encrypted waiting tasks from one
owner (default 32, accepted range 1–4096), independently of
`max_queued_tasks` (global queue maximum). Both quotas are checked in
one SQLite transaction before the task handle is issued. The owner is
an opaque task-owner fingerprint; a public, unauthenticated client may be
able to use multiple bearer identities, so this is **not** proof of
provider-authenticated user fairness or an anti-Sybil guarantee.

The owner scheduler is unweighted round-robin, not a priority scheduler:
it does not guarantee absolute global FIFO or strict completion order
across parallel workers. It never authorizes reissuing an unknown external
effect. The queue requires `REGATE_QUEUE_KEYS_JSON` to be available
when enabled.

### Queue encryption key rotation

Queue encryption keys are supplied separately from HCL through
`REGATE_QUEUE_KEYS_JSON`, with an `active` key ID and a map of retained
32-byte base64 AES-GCM keys. After adding a new active key while retaining
the old one, restart the worker and call the administrator's
`regate_queue_key_usage` and `regate_queue_rewrap` tools until no
queued entries reference the old key. Rewrapping checks the original
owner, revision, TTL and authenticated ciphertext before committing a
fresh nonce; a damaged batch is rolled back without changing task state.

Do not remove old keys until no *live* queue rows require them **and**
historical backup/recovery requirements have been reviewed. These tools
are available only through authenticated management MCP/REST, not from
public task surfaces.

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

Top-level blocks: `regate`, `oauth`, `upstream`, `hook`, `policy`, `readback`,
`compensation`, `pipeline`, `surface`, `interceptor`. Within pipelines: `node` and `edge`. Within
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

### Positive-only MCP reconciliation declarations

A top-level `readback` declaration binds an effectful MCP operation to a
trusted read-only lookup on the same upstream:

```hcl
readback "customer_update" {
  write_operation = "crm.update_customer"
  upstream        = "crm"
  read_tool       = "get_customer"
  intent_pointer  = "/customer_id"
  read_argument   = "customer_id"
  result_pointer  = "/structuredContent/customer_id"
  safe_read       = true
}
```

Both pointers are RFC 6901 JSON Pointers: the intent selector must extract a
stable identity from the original write arguments, and the result selector
must match that identity in an actual MCP read response. Explicit
`safe_read = true` is a trusted operator assertion that this particular
read tool has no external effects; it is not inferred from MCP tool
annotations. A missing or mismatched lookup never establishes absence.
Older effects without an endpoint fingerprint are not automatically
reconcilable.

### Explicit MCP compensation declarations

A `compensation` block describes one operator-reviewed inverse tool for an
original MCP write. It is not run automatically, nor does Regate infer that an
inverse is semantically safe:

```hcl
compensation "undo_customer_create" {
  for_operation = "crm.create_customer"
  upstream       = "crm"
  tool           = "delete_customer"
  intent_pointer = "/external_id"
  argument       = "external_id"
  enabled        = true
}
```

The inverse must reference the same configured upstream as the original write.
At runtime, Regate validates the original effect's endpoint and exact input
fingerprints, its `resolved_applied` status and an explicit admin-supplied
actor and reason. The original input's JSON Pointer selects the identifier
sent to the inverse operation under `argument`. Other arbitrary CEL
expressions or implicit authorization are not accepted in this declaration.

A named compensation contract is only an operator-provided assertion about
resource behavior, **not** a proof of semantic reversibility. A compensation
is journaled as a distinct one-time external effect; its successful transport
response does not rewrite or roll back the original effect.

### Source-preserving edits and semantic diffs

`regate_graph_patch` replaces one explicitly named pipeline block without
changing any bytes outside it. When the original and replacement blocks
have the same nested structure and attribute keys, it patches **only changed
attribute expressions**, preserving operator comments and unsupported text
inside the pipeline as well. `regate_attribute_patch` (and REST
`POST /api/attribute-patch`) replaces **only an existing attribute expression**
in a draft. The required `path` is a slash-separated sequence of block type
and label, e.g. `pipeline/customer_summary/node/fetch`, plus `attribute` and
`expression` (new HCL2 source). It refuses ambiguous paths, missing attributes and syntax errors. Edits
to a previously valid executable configuration remain strictly validated;
opaque drafts can remain incomplete, but cannot be activated until fixed.

The source retains comments, whitespace and opaque content outside the edit.
A **structural** graph replacement is now merged by stable block identities:
additions/removals alter only the affected source ranges and preserve operator
comments on unrelated nodes and edges. The content *within* a deliberately
removed block is deleted as part of that operation. Adding a field does not
require a whole-block replacement; use `regate_attribute_add` or REST
`POST /api/attribute-add`. Opaque draft syntax is parsed by `ParseForEdit` without evaluating unknown
expressions. Graph edits preserve unrepresented unknown attributes and blocks
byte-for-byte, reject attempts to silently mutate or introduce opaque content,
and never execute it. Explicitly unsupported executable HCL is still refused
by `Decode` before activation; retaining its source does not make it runnable.

`regate_diff` and `regate_impact_analyze` provide structural summaries and
`semantic_changes` with stable paths for added/removed/changed executable
values. Whitespace, source spans and comments alone are not semantic changes.

### Remaining limitations

The compiled IR is typed at the known HCL and CEL expression boundary, not at
all dynamic MCP port boundaries. Pending: full cross-port JSON Schema typing,
verified remote contract resolution, explicit pure data dependency edges,
fully guided editing of unknown/opaque blocks (the raw HCL editor remains
required for these), and a browser-level source-preservation round trip.
Supported node/edge and attribute additions/removals are handled by source
range operations, and opaque nested blocks remain untouched by graph edits. Dynamic CEL subscripts
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
identities or would invalidate a previously executable configuration. Drafts
that already contain unsupported HCL may still be edited while remaining
non-executable. Only explicit raw-source edits may change opaque fragments. Comments immediately
outside changed ranges are preserved. Deleted elements cannot preserve content
*inside* their own deleted range; for this reason a visual editor should warn
when deleting a node with hand-written documentation. `AddAttribute` provides
a targeted insertion operation in the underlying configuration package.

### Opaque source and draft validation boundary

The management `regate_draft_patch` operation performs **HCL syntax** validation,
not full executable compilation. This allows partially configured and
forward-compatible drafts to keep opaque blocks, unknown variables and
unknown attributes. `regate_validate` and `regate_activate` always perform
strict `Decode` before any revision may be published. Visual graph patches
cannot delete unrepresented unknown fields or nested blocks, and reject
attempts to edit these fields semantically. Comments and whitespace are
preserved by byte-range edits rather than HCL AST regeneration. A malformed
HCL token stream cannot be persisted via the structured draft-patch method.


### Deterministic fail-closed join activation (PR #43)

A `join` node always waits for **all distinct incoming predecessor
nodes** to settle before becoming eligible. Its `mode` controls the
required number of active/fired inbound edges, not when the gateway may
speculatively execute downstream tools:

- `all` (default): every declared incoming predecessor must be completed
  and its guarded edge fired.
- `any`: at least one predecessor must have fired.
- `quorum`: at least the configured `quorum` number of distinct
  predecessors must have fired.

If the threshold is not reached after full settlement, the join is
`skipped`, and any downstream nodes requiring its control edge are
skipped. A still-pending predecessor, suspension, or external-error
path cannot be ignored to start an early downstream write. HCL
validation rejects a quorum above the number of incoming branches,
quorum settings on non-quorum joins, joins with no incoming branches,
and duplicate predecessors even if their edges use different guards.
These are control-flow guarantees only: arbitrary policy placement,
upstream side effects and data compatibility still require separate
validation and security review.


### Explicit mandatory policy dominance in pipeline DAGs (PR #44)

A pipeline `node` with `kind = "policy.check"` can now declare
`mandatory = true`. This is an **opt-in authorization contract**:
the compiler requires a named policy with a non-builtin decision hook
(`contract = "decision"`) and a decision-input `value` expression.
A no-op/unspecified hook or a mandatory policy without input fails
activation.

For each such gate, the HCL compiler proves **structural dominance**:
every control-flow path from the pipeline entry to any `mcp.call`,
`hook.call`, `http.call`, `output`, or `suspend` node must traverse
the mandatory policy node. This protects external effects **and**
returnable outputs, independently of any/threshold join semantics.
Potentially true guards are treated conservatively: one alternative
route around the gate rejects the configuration even if the operator
expects the route to be rarely taken.

```hcl
node "authorize" {
  kind      = "policy.check"
  policy    = "customer_read"
  mandatory = true
  value     = "input"
}
```

Once compiled, the runtime continues to execute the existing fail-closed
Decision hook. A denial, indeterminate response, or unavailable hook
cannot activate downstream protected nodes. The dominance rule is
**per pipeline**, not a claim that direct tools on the same MCP surface
are covered by a pipeline policy; a direct tool needs its own surface
authorization and interceptors. Mandatory policy domination is distinct
from tenant/owner identity, evidence-backed grant validation, and
verified remote-effect fencing, all of which have separate contracts.
