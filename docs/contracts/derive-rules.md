# Deriving tools from repeated calls

Enable collection on a curated surface by adding these attributes to its
existing HCL block.

```hcl
derive_rules             = true
derive_session_headers   = ["X-Client-Session", "X-Conversation-ID"]
derive_session_meta_keys = ["my-client/conversation"]
```

Both source lists default to empty. Regate chooses the first unambiguous
value from the ordered HTTP headers, then from the ordered string keys in
`params._meta`. Clients can keep their existing header or metadata conventions.
An empty, repeated or comma-separated header does not supply an ID. Client
IDs are for grouping only and carry no authorization. Authorization and
cookie headers cannot be configured as sources.

Without an explicit ID, Regate groups by verified OAuth issuer, audience and
subject, starting a new session after 15 minutes of inactivity. Calls without
either source of grouping evidence execute normally and are excluded. The
journal is bounded by 30 days and 100,000 observations in the live SQLite
database. Each observation contains the surface, revision, tool, run ID,
timestamps, outcome and a keyed group digest. The digest binds the ID to its
configured source, surface, revision and verified identity when available.
Arguments, responses, tokens and raw session IDs are excluded from the
observation journal. Existing task and effect persistence follow their own
contracts.

## Find a chain and prepare a draft

In **Chains**, choose a surface and click **Find chains**. A suggestion is a
contiguous sequence of 2–5 successful calls seen in at least three distinct
sessions on that surface and revision. Failed, unfinished and overlapping
calls break sequences. Each suggestion includes counts by grouping method and
example run IDs. A tool that can suspend appears in the results after
successful execution, with draft generation disabled for its chain.

The same operations are available through the admin MCP and management REST API.

| MCP tool | REST endpoint | Arguments |
| --- | --- | --- |
| `regate_derive_rules` | `POST /api/derive-rules` | `surface`, optional `cursor` |
| `regate_derive_draft` | `POST /api/derive-draft` | `surface`, `chain_id`, `tool_name`, `expected_revision` |

Pass the returned `next_cursor` as `cursor` to fetch the next page. Use
the revision returned by the search. Draft creation conflicts if the
active revision changed or the tool or generated pipeline name is taken.
Regate inserts the tool block into the existing surface and appends a
pipeline, preserving all existing HCL bytes. A surface managed by deployment
must be edited in its deployment source. The console displays this constraint
when browsing its observed chains.

The generated input has a separate `step_1`, `step_2`, and so on, using the
source tools' declared schemas. Each node's CEL `arguments` initially reads
its corresponding input. For example, use `nodes.step_1.value` to feed the
first result to the second step. Adjust the public input schema to match your
new mappings. The initial output is the final step's result.

Use `regate_test` with `draft_id`, `expected_source_digest`, `surface`,
`tool`, `arguments` and a `mocks` object to dry-run the generated graph. Keys
in `mocks` are `pipeline.node` and values are synthetic source-tool results.
Dry runs validate each invoked tool's input and mock output against its
declared schema. They execute the surrounding graph without dispatching the
mocked tools. Review `regate_impact_analyze` for target tools, potential writes
and scopes. Publish using the ordinary explicit activation.

## Execution contract

A `tool.call` node names a tool on the enclosing tool's own curated surface.
It executes the target at the pinned revision with the calling client's
identity. The target's input and output schemas, scopes, policies, hooks,
masking and effect journal apply as in a direct public call. A parent must
already require all of the target's scopes, either at surface or tool level.
Compilation rejects cycles and suspendable targets. A node that may dispatch
an effect requires `allow_effects = true`. Generated pipelines schedule one
step at a time and stop on an error. The composition never retries a step.
An ambiguous write remains subject to ordinary effect reconciliation.
