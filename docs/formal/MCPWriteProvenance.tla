------------------------- MODULE MCPWriteProvenance -------------------------
EXTENDS Naturals, TLC

(*
  Bounded model of one MCP write journal ID, one immutable provider record,
  and a contract-bound positive-only readback. A prior provider record has
  the SAME external ID and payload but a DIFFERENT operation token.

  "app_error" is an ordinary HTTP-successful MCP isError=true response.
  "lost" also represents a process crash before response completion.
  The model allows the provider to commit after the gateway goes UNKNOWN.

  A changed adapter/upstream generates a different token and cannot be
  used to read an older UNKNOWN effect. This models only honest providers
  that atomically retain the write token; it does not prove remote fencing,
  secret authentication, provider consistency, or liveness.
*)
VARIABLES phase, attempts, provider, contract, response, reads, lastReadContract, rejected

vars == <<phase, attempts, provider, contract, response, reads, lastReadContract, rejected>>

Init ==
    /\ phase = "ready"
    /\ attempts = 0
    /\ provider \in {"empty", "prior"}
    /\ contract = "original"
    /\ response = "none"
    /\ reads = 0
    /\ lastReadContract = "none"
    /\ rejected = 0

AdmitWrite ==
    /\ phase = "ready"
    /\ attempts = 0
    /\ phase' = "inflight"
    /\ attempts' = 1
    /\ UNCHANGED <<provider, contract, response, reads, lastReadContract, rejected>>

CommitProvider ==
    /\ phase \in {"inflight", "unknown"}
    /\ provider = "empty"
    /\ provider' = "current"
    /\ UNCHANGED <<phase, attempts, contract, response, reads, lastReadContract, rejected>>

Success ==
    /\ phase = "inflight"
    /\ provider = "current"
    /\ phase' = "confirmed"
    /\ response' = "success"
    /\ UNCHANGED <<attempts, provider, contract, reads, lastReadContract, rejected>>

ApplicationError ==
    /\ phase = "inflight"
    /\ phase' = "unknown"
    /\ response' = "app_error"
    /\ UNCHANGED <<attempts, provider, contract, reads, lastReadContract, rejected>>

ResponseLost ==
    /\ phase = "inflight"
    /\ phase' = "unknown"
    /\ response' = "lost"
    /\ UNCHANGED <<attempts, provider, contract, reads, lastReadContract, rejected>>

DriftContract ==
    /\ phase \in {"inflight", "unknown"}
    /\ contract = "original"
    /\ contract' = "changed"
    /\ UNCHANGED <<phase, attempts, provider, response, reads, lastReadContract, rejected>>

RestoreContract ==
    /\ phase = "unknown"
    /\ contract = "changed"
    /\ contract' = "original"
    /\ UNCHANGED <<phase, attempts, provider, response, reads, lastReadContract, rejected>>

PositiveReadback ==
    /\ phase = "unknown"
    /\ contract = "original"
    /\ provider = "current"
    /\ reads < 2
    /\ phase' = "resolved_readback"
    /\ reads' = reads + 1
    /\ lastReadContract' = contract
    /\ UNCHANGED <<attempts, provider, contract, response, rejected>>

NegativeReadback ==
    /\ phase = "unknown"
    /\ contract = "original"
    /\ provider # "current"
    /\ reads < 2
    /\ reads' = reads + 1
    /\ lastReadContract' = contract
    /\ UNCHANGED <<phase, attempts, provider, contract, response, rejected>>

DenyChangedReadback ==
    /\ phase = "unknown"
    /\ contract = "changed"
    /\ rejected < 1
    /\ rejected' = rejected + 1
    /\ UNCHANGED <<phase, attempts, provider, contract, response, reads, lastReadContract>>

OperatorDecision ==
    /\ phase = "unknown"
    /\ phase' = "resolved_operator"
    /\ UNCHANGED <<attempts, provider, contract, response, reads, lastReadContract, rejected>>

Next ==
    \/ AdmitWrite
    \/ CommitProvider
    \/ Success
    \/ ApplicationError
    \/ ResponseLost
    \/ DriftContract
    \/ RestoreContract
    \/ PositiveReadback
    \/ NegativeReadback
    \/ DenyChangedReadback
    \/ OperatorDecision

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ phase \in {"ready", "inflight", "unknown", "confirmed",
                   "resolved_readback", "resolved_operator"}
    /\ attempts \in 0..1
    /\ provider \in {"empty", "prior", "current"}
    /\ contract \in {"original", "changed"}
    /\ response \in {"none", "success", "lost", "app_error"}
    /\ reads \in 0..2
    /\ lastReadContract \in {"none", "original", "changed"}
    /\ rejected \in 0..1

NoBlindWriteRetry == attempts <= 1

ReadbackNeedsOriginalProof ==
    (phase = "resolved_readback") =>
        (provider = "current" /\ contract = "original" /\ attempts = 1)

NoFalseSuccess ==
    (phase = "confirmed") => (response = "success" /\ provider = "current")

ApplicationErrorIsUncertain ==
    (response = "app_error") => (phase # "confirmed")

NeverReadWithDriftedContract == lastReadContract # "changed"

PriorRecordCannotResolve ==
    (provider = "prior") => (phase # "resolved_readback")

=============================================================================
