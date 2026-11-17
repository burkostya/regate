--------------------------- MODULE JoinBarrier ---------------------------
EXTENDS Naturals, FiniteSets, TLC

(*
  The Go join executes only after every control predecessor settles.
  A branch may fire its control edge, skip it, or suspend until its
  continuation is answered. Threshold is an all/any/quorum activation
  policy, never permission to proceed past an unsettled sibling.
  A completed join may admit at most one downstream external effect.
  Finite checks cover two and three independently settling branches.
*)
CONSTANTS Branches, Threshold

VARIABLES vote, accepted, phase, attempts

vars == <<vote, accepted, phase, attempts>>

Settled == \A b \in Branches : vote[b] \in {"fired", "skipped"}
Fired == {b \in Branches : vote[b] = "fired"}
HasThreshold == Cardinality(Fired) >= Threshold

Init ==
    /\ vote = [b \in Branches |-> "pending"]
    /\ accepted = [b \in Branches |-> 0]
    /\ phase = "waiting"
    /\ attempts = 0

Fire(b) ==
    /\ b \in Branches
    /\ vote[b] = "pending"
    /\ vote' = [vote EXCEPT ![b] = "fired"]
    /\ accepted' = [accepted EXCEPT ![b] = @ + 1]
    /\ UNCHANGED <<phase, attempts>>

Skip(b) ==
    /\ b \in Branches
    /\ vote[b] = "pending"
    /\ vote' = [vote EXCEPT ![b] = "skipped"]
    /\ accepted' = [accepted EXCEPT ![b] = @ + 1]
    /\ UNCHANGED <<phase, attempts>>

Suspend(b) ==
    /\ b \in Branches
    /\ vote[b] = "pending"
    /\ vote' = [vote EXCEPT ![b] = "suspended"]
    /\ UNCHANGED <<accepted, phase, attempts>>

ResumeFire(b) ==
    /\ b \in Branches
    /\ vote[b] = "suspended"
    /\ vote' = [vote EXCEPT ![b] = "fired"]
    /\ accepted' = [accepted EXCEPT ![b] = @ + 1]
    /\ UNCHANGED <<phase, attempts>>

ResumeSkip(b) ==
    /\ b \in Branches
    /\ vote[b] = "suspended"
    /\ vote' = [vote EXCEPT ![b] = "skipped"]
    /\ accepted' = [accepted EXCEPT ![b] = @ + 1]
    /\ UNCHANGED <<phase, attempts>>

Activate ==
    /\ phase = "waiting"
    /\ Settled
    /\ HasThreshold
    /\ phase' = "active"
    /\ UNCHANGED <<vote, accepted, attempts>>

Reject ==
    /\ phase = "waiting"
    /\ Settled
    /\ ~HasThreshold
    /\ phase' = "skipped"
    /\ UNCHANGED <<vote, accepted, attempts>>

Dispatch ==
    /\ phase = "active"
    /\ attempts = 0
    /\ phase' = "dispatched"
    /\ attempts' = attempts + 1
    /\ UNCHANGED <<vote, accepted>>

Confirm ==
    /\ phase = "dispatched"
    /\ phase' = "published"
    /\ UNCHANGED <<vote, accepted, attempts>>

LoseResponse ==
    /\ phase = "dispatched"
    /\ phase' = "unknown"
    /\ UNCHANGED <<vote, accepted, attempts>>

Next ==
    \/ (\E b \in Branches : Fire(b))
    \/ (\E b \in Branches : Skip(b))
    \/ (\E b \in Branches : Suspend(b))
    \/ (\E b \in Branches : ResumeFire(b))
    \/ (\E b \in Branches : ResumeSkip(b))
    \/ Activate
    \/ Reject
    \/ Dispatch
    \/ Confirm
    \/ LoseResponse

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ Threshold \in 1..Cardinality(Branches)
    /\ vote \in [Branches -> {"pending", "suspended", "fired", "skipped"}]
    /\ accepted \in [Branches -> Nat]
    /\ phase \in {"waiting", "active", "skipped", "dispatched", "published", "unknown"}
    /\ attempts \in Nat

NoDuplicateVotes == \A b \in Branches : accepted[b] <= 1
DurableVotePresence ==
    \A b \in Branches :
        (accepted[b] = 1) <=> (vote[b] \in {"fired", "skipped"})
NoPrematureJoin == (phase # "waiting") => Settled
NoFalseActivation ==
    (phase \in {"active", "dispatched", "published", "unknown"}) => HasThreshold
NoFalseRejection == (phase = "skipped") => ~HasThreshold
NoBlindEffectRetry == attempts <= 1
NoEffectWithoutJoin ==
    (attempts = 1) =>
        (Settled /\ HasThreshold /\
         phase \in {"dispatched", "published", "unknown"})
NoPrematurePublication == (phase = "published") => (Settled /\ HasThreshold)

=============================================================================
