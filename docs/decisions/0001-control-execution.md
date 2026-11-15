# ADR-0001: Separate source, execution and presentation models

- Status: accepted architectural direction, partial prototype implementation
- Context: both agents and humans need to modify virtual MCP surfaces and execution rules without corrupting unrelated settings.
- Decision: HCL is the sole declarative source of execution rules. An explicit compiler produces a validated immutable execution model; visual positions and grouping live in separate JSON. UI, CLI and admin MCP share a draft→validate→test→diff→activate workflow.
- Consequences: safe concurrent editing, repeatable deployments and compatibility checks; source patches must preserve unknown fragments and fail on ambiguous edits.
- Prototype deviation: the parser is restricted HCL, the execution graph is not a distinct typed IR, and AST patching preserves bytes only *outside* replaced blocks.
