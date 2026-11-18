# Formal model checking

GitHub CI runs the pinned TLA+ TLC 1.8.0 model checker (SHA-256
`7e53aa636dac8a3cb0533f343115c0548e03fc0ce5edc7739becf58d3d0fb84f`)
against three **finite** safety models (with two- and three-branch configurations):

- `EffectLifecycle.tla` with `EffectLifecycle.cfg`: effect admission,
  uncertain outcomes, and the no-blind-write-retry invariant.
- `MCPWriteProvenance.tla` with `MCPWriteProvenance.cfg`: one
  resource-bound MCP effect, a prior record with the **same external ID and
  payload** but different operation token, provider commit before/after
  an uncertain response, standard MCP post-commit application error,
  lost transport response, changed HCL contract, denied readback without
  network I/O, positive readback and explicit operator decision. Checks
  no blind write retry, no false confirmed result, no wrong-token applied
  verdict, and no read with a drifted adapter. Matching Go tests:
  `internal/engine/mcp_tool_outcome_test.go`,
  `internal/engine/readback_test.go` and
  `internal/server/two_process_mcp_stand_test.go`.
- `JoinSafety.tla` with five `JoinSafety*.cfg` configurations: explicit
  two/three-predecessor all/any/quorum settlement barriers, independent
  authorization, no ambiguous write retry, and the static guaranteed-source
  threshold rule. Go runtime truth tables in
  `internal/engine/join_model_test.go` exercise matching branch outcomes.
- `ParallelSuspensions.tla` with `ParallelSuspensions.cfg`: two independent
  effectful branches, serialized targeted continuation, invalid replies,
  worker crash during a claimed answer, and publication only after every
  leaf is durably answered.

Run locally with a Java 11+ runtime and the pinned release's `tla2tools.jar`:

```sh
cd docs/formal
java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config EffectLifecycle.cfg EffectLifecycle.tla
java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config MCPWriteProvenance.cfg MCPWriteProvenance.tla
java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config ParallelSuspensions.cfg ParallelSuspensions.tla
java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config ParallelSuspensions3.cfg ParallelSuspensions.tla
for c in JoinSafetyAny2 JoinSafetyAll2 JoinSafetyAny3 JoinSafetyQuorum3 JoinSafetyAll3; do
  java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config "$c.cfg" JoinSafety.tla
done
```

The `-deadlock` switch suppresses terminal-state warnings for intentionally
finished models; all listed safety invariants are still checked for every
reachable state. Changing a model or its `.cfg` requires CI to pass.

**Scope limitation:** These are exhaustive checks only within their explicitly
bounded abstractions, not correctness proofs of the Go implementation or
of external MCP/HTTP provider behavior. Still required for full Regate v1:
model-based integration/property tests linking runtime traces to the model;
graph readiness/joins, cancellation, revision isolation, capability
delegation/taint, realistic idempotency contracts, restart/reconciliation,
load/failure/security testing, and independent provider conformance.
