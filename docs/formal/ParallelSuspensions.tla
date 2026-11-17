----------------------- MODULE ParallelSuspensions -----------------------
EXTENDS Naturals, FiniteSets, TLC

(*
  Bounded abstraction of Regate's per-run continuation guard and durable
  parallel suspend branches. Remote writes are admitted at most once per
  branch; neither invalid inputs nor worker crashes discard accepted replies.
  Downstream publication happens only after the full input barrier settles.
  This is a safety model, not a provider exactly-once or liveness proof.
*)
CONSTANTS Branches, Clients

VARIABLES phase, stage, attempts, accepted, holder, selected, published

vars == <<phase, stage, attempts, accepted, holder, selected, published>>

Init ==
    /\ phase = "collecting"
    /\ stage = [b \in Branches |-> "ready"]
    /\ attempts = [b \in Branches |-> 0]
    /\ accepted = [b \in Branches |-> 0]
    /\ holder = "free"
    /\ selected = "none"
    /\ published = FALSE

Dispatch(b) ==
    /\ b \in Branches
    /\ phase = "collecting"
    /\ stage[b] = "ready"
    /\ attempts[b] = 0
    /\ stage' = [stage EXCEPT ![b] = "pending"]
    /\ attempts' = [attempts EXCEPT ![b] = @ + 1]
    /\ UNCHANGED <<phase, accepted, holder, selected, published>>

Park ==
    /\ phase = "collecting"
    /\ \A b \in Branches : stage[b] = "pending"
    /\ phase' = "waiting"
    /\ UNCHANGED <<stage, attempts, accepted, holder, selected, published>>

Acquire(c, b) ==
    /\ phase = "waiting"
    /\ c \in Clients
    /\ b \in Branches
    /\ holder = "free"
    /\ stage[b] = "pending"
    /\ holder' = c
    /\ selected' = b
    /\ UNCHANGED <<phase, stage, attempts, accepted, published>>

Reject(c) ==
    /\ c \in Clients
    /\ holder = c
    /\ holder' = "free"
    /\ selected' = "none"
    /\ UNCHANGED <<phase, stage, attempts, accepted, published>>

Answer(c) ==
    /\ c \in Clients
    /\ holder = c
    /\ selected \in Branches
    /\ stage[selected] = "pending"
    /\ stage' = [stage EXCEPT ![selected] = "answered"]
    /\ accepted' = [accepted EXCEPT ![selected] = @ + 1]
    /\ holder' = "free"
    /\ selected' = "none"
    /\ UNCHANGED <<phase, attempts, published>>

Crash ==
    /\ holder \in Clients
    /\ holder' = "free"
    /\ selected' = "none"
    /\ UNCHANGED <<phase, stage, attempts, accepted, published>>

Publish ==
    /\ phase = "waiting"
    /\ holder = "free"
    /\ \A b \in Branches : stage[b] = "answered"
    /\ ~published
    /\ phase' = "completed"
    /\ published' = TRUE
    /\ UNCHANGED <<stage, attempts, accepted, holder, selected>>

Next ==
    \/ (\E b \in Branches : Dispatch(b))
    \/ Park
    \/ (\E c \in Clients : \E b \in Branches : Acquire(c, b))
    \/ (\E c \in Clients : Reject(c))
    \/ (\E c \in Clients : Answer(c))
    \/ Crash
    \/ Publish

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ phase \in {"collecting", "waiting", "completed"}
    /\ stage \in [Branches -> {"ready", "pending", "answered"}]
    /\ attempts \in [Branches -> Nat]
    /\ accepted \in [Branches -> Nat]
    /\ holder \in Clients \cup {"free"}
    /\ selected \in Branches \cup {"none"}
    /\ published \in BOOLEAN

NoBlindWriteRetry == \A b \in Branches : attempts[b] <= 1
NoDuplicateAcceptedReply == \A b \in Branches : accepted[b] <= 1
DurableAnswerPresence ==
    \A b \in Branches : (stage[b] = "answered") <=> (accepted[b] = 1)
NoPrematurePublication ==
    (published <=> (phase = "completed")) /\
    (published => (\A b \in Branches : stage[b] = "answered"))
NoResumeBeforeWaveSettles ==
    (holder # "free") => (phase = "waiting" /\
        (\A b \in Branches : stage[b] # "ready"))

=============================================================================
