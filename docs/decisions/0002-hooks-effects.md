# ADR-0002: Hooks do not replace authorization or effect contracts

- Status: accepted architectural direction, partial prototype implementation
- Context: external PII scrubbers, LLM agents and Decision APIs can modify or disclose data, and network failures can leave uncertain write outcomes.
- Decision: callbacks/hooks have typed contracts and are placed in a governed graph; mandatory policy checks cannot be bypassed. Every potentially effectful call must have an effect contract and durable journal, with reconciliation rather than blind retry on unknown results.
- Consequences: authoring requires explicit trust/data-flow boundaries; availability may degrade safely without allowing protected effects. No hook can unilaterally grant new authority.
- Prototype deviation: hooks and a dispatching/unknown journal exist, but there is no generalized effect-contract verifier, reconciliation coordinator or data label enforcement yet.
