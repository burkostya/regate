# ADR-0001: Separate source, execution and presentation models

- Status: accepted architectural direction, partial prototype implementation
- Context: both agents and humans need to modify virtual MCP surfaces and execution rules without corrupting unrelated settings.
- Decision: HCL is the sole declarative source of execution rules. An explicit compiler produces a validated immutable execution model; visual positions and grouping live in separate JSON. UI, CLI and admin MCP share a draft→validate→test→diff→activate workflow.
- Consequences: safe concurrent editing, repeatable deployments and compatibility checks; source patches must preserve unknown fragments and fail on ambiguous edits.
- Prototype deviation after compiler migration: HCL2 and CEL compile into a separate typed IR, but dynamic cross-port JSON Schema verification is incomplete, and whole-block patching preserves bytes only *outside* replaced blocks. Attribute-level edits are byte-preserving.
