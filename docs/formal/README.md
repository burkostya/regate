# Formal models

`EffectLifecycle.tla` is a minimal TLA+ safety sketch. It models planned → dispatched → confirmed/unknown → resolved transitions and checks that there is never a blind retry of an unknown external write. **It has not been executed with TLC**; treat it as a design artifact rather than a proof.

For complete Regate v1, add/check models of graph readiness/joins under cancellation, revision activation and concurrency, capability delegation, data labels and trust boundaries, idempotency fencing, task restart/recovery, and effect reconciliation. Counterexamples should be converted into automated integration/property tests.
