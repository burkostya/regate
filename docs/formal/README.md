# Formal model checking

GitHub CI runs the pinned TLA+ TLC 1.8.0 model checker (SHA-256
`7e53aa636dac8a3cb0533f343115c0548e03fc0ce5edc7739becf58d3d0fb84f`)
against two **finite** safety models:

- `EffectLifecycle.tla` with `EffectLifecycle.cfg`: effect admission,
  uncertain outcomes, and the no-blind-write-retry invariant.
- `ParallelSuspensions.tla` with `ParallelSuspensions.cfg`: two independent
  effectful branches, serialized targeted continuation, invalid replies,
  worker crash during a claimed answer, and publication only after every
  leaf is durably answered.

Run locally with a Java 11+ runtime and the pinned release's `tla2tools.jar`:

```sh
cd docs/formal
java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config EffectLifecycle.cfg EffectLifecycle.tla
java -XX:+UseParallelGC -jar /path/to/tla2tools.jar -workers 1 -deadlock -config ParallelSuspensions.cfg ParallelSuspensions.tla
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
