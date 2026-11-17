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


### Explicit named data ports (PR #45)

A `node` may declare `input_ports`, a map of port identifiers to inline JSON
Schema 2020-12 contracts. Expressions receive a separate `ports` CEL variable.
The existing `nodes.<name>.value` and `input` expressions remain supported.

```hcl
node "normalize" {
  kind = "transform"
  value = "literal:ready"
  output_schema = {type = "string"}
}
node "finish" {
  kind = "output"
  input_ports = {result = {type = "string"}}
  value = "ports.result"
}
edge { # control order is still mandatory (write attributes on separate lines)
  from = "normalize"
  to   = "finish"
}
edge {
  kind      = "data"
  from      = "normalize"
  to        = "finish"
  from_port = "value"
  to_port   = "result"
}
```

Legacy edges (no `kind`) and `kind = "control"` remain **control edges**.
A `data` edge binds an immutable completed producer's `value` into one
declared consumer input port; it does not fire a control branch or affect join
quorum counts. Exactly one producer per named input port is required.
Only `from_port = "value"` (also the default) is currently supported. A
data edge cannot have a `when` guard or override a control edge. Data
sources must be guaranteed to complete before a consumer activates. The
compiler uses the same distinct-branch thresholds as the scheduler: with an
`all` join both incoming producer values are guaranteed; with `any` or
`quorum`, a producer is guaranteed only if every viable winning combination
includes its execution. Guards are ignored conservatively. Bypassed, missing,
duplicate and cyclic dependencies are rejected at activation; control-edge
duplicates are rejected too.

Known incompatible producer/output and consumer/port JSON types fail at
compile time. Every bound value is schema-validated at runtime **before**
the consumer executes an effect or hook; errors do not publish that node's
result. Dynamic remote output remains untrusted, and this is not a proof of
arbitrary JSON Schema subsumption, cross-branch atomicity or remote effect
correctness. Nested pipeline composition and multi-output ports remain open.


### Threshold-aware data availability (PR #46)

The compiler now checks ordinary `nodes.producer.value` CEL references as well
as explicit `data` ports. Merely being an ancestor is insufficient if an
alternative branch can activate the consumer without producing the value.
The check requires that the producer **completed** whenever the consumer
activates, including after resumed suspension. It distinguishes normal merges
(1-of-N), `join(mode="all")` (N-of-N), `join(mode="any")` (1-of-N),
and `join(mode="quorum", quorum=k)` (k-of-N).

The analysis propagates guaranteed completion sets in topological order.
For a join threshold k among n distinct predecessors, a predecessor's
guarantees are retained if they occur in at least n−k+1 incoming paths. This
is sound even when edge guards may be false, because the runtime never fires
the join without k distinct activated predecessors and never speculates past
unsettled branches.

A protected consumer may take data ports from both branches of an `all`
join; an `any` join cannot read an optional branch's value without an
explicit optional-data contract (not implemented yet). No data edge counts
as a branch vote. This proves local value availability, **not** remote
side-effect atomicity, policy attestation, or a provider's schema correctness.


### Bounded pure map nodes (PR #47)

A pipeline `node` with `kind = "map"` evaluates a pure CEL mapping on a
bounded array. `items` is evaluated **once** against the pipeline's input,
completed nodes and bound ports. For each item, `mapping` is evaluated
with CEL `input` referring to **that item**; the completed `nodes` snapshot
and named `ports` are unchanged. The resulting array is the node's `value`.

```hcl
node "upper_names" {
  kind      = "map"
  items     = "input.names"
  mapping   = "upper(input)"
  max_items = 32
  input_schema  = {type = "string"} # contract for each input item
  output_schema = {
    type = "array"
    items = {type = "string"}
  }
}
```

The operator must set `max_items` between 1 and 64. Inputs must evaluate
to an array and cannot exceed that bound; zero items produce an empty array.
Every element is validated with `input_schema` (when present), then
transformed using bounded, cancellable CEL. The complete output array is
checked against `output_schema` before it becomes visible downstream.
Invalid elements, runtime CEL errors and oversize arrays fail the node
without publishing partial results.

This node makes **no external calls in the per-item loop**. Mapping an
HTTP/MCP agent subpipeline, retries and effectful loops need separate effect
identities, fencing, retry contracts and explicit authorization; they are
not implemented by `kind = "map"`.


### Composable pure subpipelines (PR #48)

A `pipeline.call` node invokes a named, immutable HCL pipeline within the
current execution. The callee has an `input` entry and exactly one `output`
node; `arguments` is an object (literal mapping or CEL returning an object),
and the output value becomes the parent call node's `value`. Call-level
`input_schema`/`output_schema` and callee input/output schemas enforce the
interface; statically obvious mismatches are rejected before activation.

```hcl
node "run_cleaner" {
  kind      = "pipeline.call"
  pipeline  = "normalize_customer"
  arguments = {name = "input.customer_name"}
  input_schema = {type = "object"}
}
```

Only **pure** callees are allowed in this milestone: `input`, `output`,
`transform`, `filter`, `validate`, `condition`, `parallel`,
`join`, bounded `map`, `mask.pii`, and other pure `pipeline.call` nodes.
The compiler rejects transitive `policy.check`, hooks, MCP/HTTP calls and
`suspend` in callee graphs. Cycles are forbidden and maximum call depth is 8,
including the calling pipeline. A child can have branch guards; if its
output node is not activated, the parent call fails closed rather than
publishing a missing result. Child runtimes enforce the callee timeout and
existing bounded graph concurrency. Nested trace identifiers are derived
deterministically from parent run + call identity; the outer run ID remains
unchanged.

This is **not** effectful nested composition, multi-output contracts, a
recursive suspension/checkpoint model, or an assertion that operator
policy/PII obligations follow arbitrary data across tenants. External
effects in subpipelines require a separate durable effect identity and
authoritative recovery protocol.


### Explicit MCP structured output port (PR #49)

`mcp.call` can expose a separately checked `structured` data port.
An `output_ports.structured` JSON Schema contract is required on the
producer. The data edge must name `from_port = "structured"`; its consumer
must separately declare the matching `input_ports` schema. Both contracts
are checked for known incompatibilities at compile time and for actual
MCP `structuredContent` at runtime. Missing `structuredContent` is an
error even when the consumer schema would otherwise allow null.

```hcl
node "lookup" {
  kind     = "mcp.call"
  upstream = "crm"
  tool     = "get_customer"
  effect   = "read"
  arguments = {id = "input.id"}
  output_ports = {
    structured = {
      type = "object"
      required = ["id"]
      properties = {id = {type = "string"}}
    }
  }
}
node "finish" {
  kind = "output"
  input_ports = {customer = {type = "object"}}
  value = "ports.customer"
}
edge {
  from = "lookup"
  to   = "finish"
}
edge {
  kind      = "data"
  from      = "lookup"
  from_port = "structured"
  to        = "finish"
  to_port   = "customer"
}
```

`output_ports.value` is an optional extra contract for the ordinary node
value. Existing `output_schema` and implicit `from_port = "value"` data
edges remain valid. `structured` is *only* supported for MCP calls with an
explicit schema; raw `mcp` envelopes, status and metadata are not implicitly
exposed as typed ports. Dry-run MCP nodes can supply `mock_structured`
alongside the existing `mock` to exercise this boundary without an upstream.
Neither declared MCP annotations nor remote claims are trusted as a schema
attestation. When an after-call interceptor rewrites the MCP result, raw
structured content is invalidated rather than silently reused.


### Optional data ports and explicit presence (PR #50)

An `any`/`quorum` join may activate with a skipped data source. Such
dependencies remain **forbidden by default**. A consumer can explicitly
opt in to a nullable, schema-checked input:

```hcl
node "merge" {
  kind = "join"
  mode = "any"
  input_ports = {
    left = {
      optional = true
      schema = {type = ["string", "null"]}
    }
  }
  value = "port_present.left ? ports.left : 'fallback'"
}
edge {
  kind = "data"
  from = "left"
  to = "merge"
  to_port = "left"
}
```

This wrapper is distinct from the existing direct JSON Schema form. It must
contain **exactly** `optional = true` and a `schema` object that admits
`null`; malformed wrappers and nonnullable optional contracts are rejected
at compilation. Direct JSON Schema declarations remain required ports.

The source must still be a control ancestor of the consumer, even for an
optional port: the scheduler guarantees the source has either *completed*
or *skipped* before the consumer activates. Only a **skipped producer** binds
`ports.left = null` with `port_present.left = false`. If it completes,
`port_present.left = true` even when the value is itself JSON null. A
completed MCP producer with missing `structuredContent`, incomplete node,
or a pending producer is **never** converted into an absent optional value.
An optional port does not create control activation, bypass an authorization
gate, or relax the node's output or effect contracts.

`port_present` is available within CEL mapping and pure map iteration,
with static port-name checking identical to `ports`. Direct unsafe
`nodes.maybe.value` expressions remain rejected for skipped branches; use
an explicit optional binding when data may be absent.


### Explicit effectful subpipeline admission (PR #51)

`pipeline.call` defaults to **pure only** as in #48. To invoke an effectful
child, the call node must declare `allow_effects = true` in HCL. The
compiler checks the transitive call graph: pure callers cannot hide effects
inside further nested calls, and all recursion and depth limits still apply.

```hcl
node "write_via_child" {
  kind          = "pipeline.call"
  pipeline      = "write_customer"
  allow_effects = true
  arguments     = "input"
}
```

Every nested call has a stable, deterministic child run ID derived from its
parent run, call-node name and callee. The child inherits execution revision,
authorization/grant/queue context and its own timeout. Child MCP writes and
external HTTP/MCP hooks use the **same child effect scope**; the journal
enforces atomic write admission and blocks replay after an unknown or already
resolved effect. An audit trace links the outer call to the derived subrun.
The parent receives only the completed child output.

Mandatory Decision gates in the parent treat an effectful
`pipeline.call` as an observable side-effect boundary; a bypass path cannot
invoke a child before the gate. Each child independently enforces its
declared mandatory gates. Unless the additional #52 contract below is
explicitly enabled, nested `suspend` is rejected. Already journaled
effects of a failing child remain available for reconciliation (not silently
rolled back).

This opt-in does **not** make operator-declared `effect="read"` a verified
idempotence proof, permit automatic retries, or grant new OAuth capabilities.
Effect classification, trust policy, provider response semantics and recovery
still require their own evidence.

Authenticated surface trust checks and queued-task OAuth delegation detection
walk **all transitive called pipelines**, including shared subpipelines. Missing
targets or recursive runtime mutations fail closed. A child writing Restricted
data requires the same explicit trusted destination as a top-level write.

### One bounded nested suspension chain (PR #52)

An effectful `pipeline.call` may **explicitly** admit child suspension with
`allow_suspend = true`. The same call must declare `allow_effects = true`.
Every enclosing `pipeline.call` on the route to the suspension must opt in;
a pure call never delegates suspension or effects. Call graphs stay acyclic and
bounded to eight pipeline frames.

```hcl
node "review_customer" {
  kind          = "pipeline.call"
  pipeline      = "create_then_request_approval"
  arguments     = "input"
  allow_effects = true
  allow_suspend = true
}
```

The runtime carries one active nested checkpoint chain to the **root run**'s
SQLite `suspended` record (subrun IDs are derived and are not separate root
checkpoint keys). The checkpoint binds immutable revision, surface, tool,
callee input arguments, completed steps, branch activations and the suspended
leaf. On `tasks/update`, Regate validates the original authorization and
revision, walks the exact call chain, validates the supplied leaf value with
the leaf `output_schema` and declared `value` port, then resumes each
parent in order. An effectful step completed before suspension is not
redispatched: its node output is reused, and durable effect admission still
blocks all confirmed/unknown duplicate writes within the logical run.

A parallel execution wave is fully settled before a checkpoint is published.
Successful sibling results and their effect identities are recorded even if
a different sibling suspends. **Two simultaneous suspension nodes are rejected
without publishing an ambiguous checkpoint**; do not schedule independent
user-input branches as parallel suspensions. If a sibling fails or the
checkpoint cannot be written, the run fails closed. Suspended states are not
automatic retries: crash-interrupted *working* tasks, remotely ambiguous
writes, OAuth refresh and operator-grant recovery still require their own
authorization/reconciliation protocols. This is deliberately not a general
multi-suspension scheduler or production-grade encrypted checkpoint vault.

### Operator-attested bounded MCP read retry (PR #53)

There is **no implicit retry** for any MCP tool call. A single pipeline
`mcp.call` node can request a small, bounded retry window *only* when it
declares `effect = "read"` and an explicit operator acknowledgement that
calling the tool repeatedly is safe:

```hcl
node "read_customer" {
  kind      = "mcp.call"
  upstream  = "crm"
  tool      = "get_customer"
  effect    = "read"
  arguments = "input"
  retry = {
    safe_read    = true
    max_attempts = 3
    delay_ms     = 50
  }
}
```

The `retry` object is closed: all three fields are mandatory, no unknown
fields are permitted, `max_attempts` must be 2–4 **including the first
attempt**, and `delay_ms` must be 1–2000. An `unknown`, `write`, or
`external_transfer` effect cannot opt in. Operator-supplied `effect="read"`
and `safe_read=true` constitute **an assertion of read-only repeat safety**;
Regate does not derive or verify that property from a provider's tool name
or an untrusted MCP metadata field.

The retry loop is restricted to transient network/connection failures
(EOF, connection reset, and Go network errors). It does not retry an MCP
application-level error response, schema failure, denied policy, OAuth
exchange failure, malformed output, HTTP hook or external write. Before each
new dispatch it waits for the configured delay with cancellation support,
then checks queued-task cancellation, delegated-grant revocation and the
execution leader lease again. Every SDK request gets a fresh MCP session; SDK
automatic retries remain disabled. The parent pipeline context bounds total
duration. No backoff is interpreted as a license to replay an unknown write.

This milestone is **not** generic workflow retry nodes, compensating writes,
idempotent-write verification, attested provider semantics or an automatic
recovery mechanism for interrupted tasks. Unsafe operator classification may
still cause a real side effect to repeat, so production deployments require
provider-specific verification and audit before enabling a read retry.

### Explicit typed MCP content output port (PR #54)

An `mcp.call` exposes the upstream's standard `content` array separately
from the existing `structuredContent` and the node's default `value`.
Consumers may bind `edge.from_port = "content"` to a declared input port;
the source must explicitly declare `output_ports.content` with a JSON
Schema whose top-level `type` is exactly `array`.

```hcl
node "fetch" {
  kind      = "mcp.call"
  upstream  = "crm"
  tool      = "lookup"
  effect    = "read"
  arguments = "input"
  output_ports = {
    content = {
      type = "array"
      items = {type = "object"}
    }
  }
  mock = {id = "C123"}
  mock_content = [{type = "text", text = "Customer C123"}]
}
node "consume" {
  kind = "output"
  input_ports = {
    blocks = {
      type = "array"
      items = {type = "object"}
    }
  }
  value = "ports.blocks[0].text"
}
edge {
  from = "fetch"
  to   = "consume"
}
edge {
  kind = "data"
  from = "fetch"
  from_port = "content"
  to   = "consume"
  to_port = "blocks"
}
```

The producer's `content` and the consumer's named input JSON Schemas
are checked for known incompatibility at compilation and checked against
actual values at runtime. A missing content field, malformed array or
invalid item fails the node before downstream effects or output release.
Declaring `content` on a non-MCP producer, or binding it without a
matching source contract, is rejected. Dry runs use explicit
`mock_content` (independent of `mock_structured`). If an
`after_call` interceptor transforms the ordinary node result, the
original MCP envelope is discarded and named raw ports fail closed
rather than exposing stale pre-interceptor content.

`content` is the original MCP SDK content-block array, **not** an
automatic text concatenation, decoding of image/audio blocks, or a
multi-output contract for `pipeline.call`. Typed MCP content and
structured ports may coexist but have separate requirements.

### MCP tool-level error boundary (PR #55)

An HTTP/JSON-RPC-successful `tools/call` response may still contain the
MCP-standard `isError = true`, meaning the **tool** has reported an
application-level failure. When this occurs inside a pipeline `mcp.call`
or as a Regate MCP hook, the node now **fails closed** before validating or
publishing output ports and before activating downstream steps. A Decision
hook is not allowed to return `allow` using the same failing envelope, even
if its `structuredContent` resembles a successful response. A malformed
`isError` flag is rejected.

For a transparent MCP proxy surface, the standard `isError` envelope is
still returned intact to its MCP client; Regate does not replace the remote
tool's valid MCP error result with a transport failure. The retry contract
in PR #53 does not retry MCP application-level errors.

A tool-level error must **not** be interpreted as proof that a previous
effectful request did nothing: writes already admitted to the durable
effect journal remain recorded and cannot be blindly replayed or refunded.
This is fail-closed pipeline execution, **not** remote transaction rollback,
provider idempotence proof or effect compensation.


### Named output ports of a nested pipeline call

A `pipeline.call` may publish explicit named `output_ports` in addition
to its legacy `value` port (the entire returned value). A child still has
**one** `output` node; that node returns an object and declares a JSON Schema
`output_schema` with `type = "object"`, direct `properties`, and `required`
for every name exported by its caller. Each caller declares its own JSON
Schema for each named `output_ports` entry, and each downstream consumer
declares an `input_ports` entry. A `kind = "data"` edge binds them with
`from_port` and `to_port`. An output such as
`{first = "input.name", second = "input.code"}` can publish both fields as
separate data edges, while `nodes.call.value` remains the whole object.

Unknown names, missing required child properties, invalid schemas and provably
incompatible producer/consumer types are rejected at compilation. Every
child result and parent named port is independently validated at runtime
before a downstream node can be activated. Missing fields, including when
a child skips its sole output, fail closed; no partial named output is
published. The same contract applies to pure and explicitly authorized
effectful/suspendable `pipeline.call` frames; checkpoint and effect identities
are unchanged. MCP `structured`/`content` envelope ports remain distinct
from named subpipeline properties. A schema using indirect `$ref` or computed
properties without an explicit, required direct property is not accepted as
proof of a published name. This feature does not yet support multiple
independent child `output` nodes or concurrent nested suspension points.


### Multiple terminal output nodes in a called pipeline

As a separate extension to the single-object named ports above, a called
pipeline may have **multiple** distinct `output` nodes. Every terminal's node
name becomes a **required** named result port of its `pipeline.call`. Each
terminal must declare an explicit `output_schema` and the caller must
declare a matching `output_ports.<terminal>` JSON Schema. No undeclared or
renamed terminal is accepted. The reserved name `value` is unavailable to
multi-terminal child outputs.

After all child control branches settle, the caller constructs a single
object `{terminal_name: validated_value, ...}`, available through the
default `nodes.call.value` and through independently typed named data ports.
Skipped terminals, invalid/absent persisted output records, or incompatible
schemas fail before downstream effects. An optional caller
`output_schema` must accept this object. Null values count as produced only
when they pass the declared schema. On checkpoint resume, already completed
sibling outputs are recovered from the stored state, not rerun.

The contract currently requires **all** declared terminals to complete, even
if their control paths are conditional; conditional alternatives should
produce a single child object with explicit nullable/optional fields instead.
Independent simultaneous suspensions and per-terminal optionality remain
future work. Existing single-output `pipeline.call` semantics are unchanged.


### Exclusive root tool output after a settled control graph

A published tool may use multiple conditional `output` nodes to model
mutually exclusive response branches. Exactly **one active completed output**
is permitted for a single MCP response. After all control predecessors
settle, if two or more root outputs complete, execution fails closed with
stable, sorted terminal names. No arbitrarily last-finished result, partial
MCP envelope or successful task response may escape. Zero output terminals
retain the existing tool-contract behavior (and still undergo the tool's
own `output_schema` validation if declared).

This rule is enforced on both initial execution and durable root
`Resume`, including outputs completed before a suspension and restored
from checkpoint state. An already consumed resume checkpoint is cleared
even when the newly completed root is rejected for competing outputs,
preventing an authorized continuation from being replayed to seek a
different result. This does not undo already dispatched external effects.

**Exception:** named multi-terminal *called* pipelines deliberately
aggregate all required child outputs into a typed object at their call
boundary. They do not publish multiple independent MCP responses.
