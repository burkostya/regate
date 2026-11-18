--------------------------- MODULE JoinSafety ---------------------------
EXTENDS Naturals, FiniteSets, TLC

(*
  Finite abstraction of Regate's control-flow joins. A join never speculates
  past an unsettled control predecessor, even when "any" or "quorum" already
  has enough fired votes. Policy must allow before the protected effect; an
  uncertain remote write is never blindly dispatched a second time.

  Threshold=1 models "any", Threshold=Cardinality(Branches) models "all",
  and intermediate values model "quorum". Votes are by distinct branch ID.
  This is not a proof about real upstream servers, cancellation, or Go code.
*)
CONSTANTS Branches, Threshold

ASSUME /\ Branches # {}
       /\ Threshold \in 1..Cardinality(Branches)

VARIABLES votes, policy, join, effect, attempts, published
vars == <<votes, policy, join, effect, attempts, published>>

Settled == \A b \in Branches : votes[b] # "pending"
Fired == {b \in Branches : votes[b] = "fired"}
ThresholdMet == Cardinality(Fired) >= Threshold

Init ==
    /\ votes = [b \in Branches |-> "pending"]
    /\ policy = "unchecked"
    /\ join = "waiting"
    /\ effect = "idle"
    /\ attempts = 0
    /\ published = FALSE

Vote(b, result) ==
    /\ b \in Branches
    /\ result \in {"fired", "skipped"}
    /\ votes[b] = "pending"
    /\ votes' = [votes EXCEPT ![b] = result]
    /\ UNCHANGED <<policy, join, effect, attempts, published>>

Authorize(verdict) ==
    /\ verdict \in {"allow", "deny"}
    /\ policy = "unchecked"
    /\ policy' = verdict
    /\ UNCHANGED <<votes, join, effect, attempts, published>>

DecideJoin ==
    /\ join = "waiting"
    /\ Settled
    /\ policy # "unchecked"
    /\ join' = IF policy = "allow" /\ ThresholdMet
               THEN "enabled" ELSE "blocked"
    /\ UNCHANGED <<votes, policy, effect, attempts, published>>

Dispatch ==
    /\ join = "enabled"
    /\ effect = "idle"
    /\ attempts = 0
    /\ attempts' = attempts + 1
    /\ effect' = "inflight"
    /\ UNCHANGED <<votes, policy, join, published>>

Unknown ==
    /\ effect = "inflight"
    /\ effect' = "unknown"
    /\ UNCHANGED <<votes, policy, join, attempts, published>>

Confirm ==
    /\ effect = "inflight"
    /\ effect' = "confirmed"
    /\ UNCHANGED <<votes, policy, join, attempts, published>>

Publish ==
    /\ join = "enabled"
    /\ effect = "confirmed"
    /\ ~published
    /\ published' = TRUE
    /\ UNCHANGED <<votes, policy, join, effect, attempts>>

Next ==
    \/ (\E b \in Branches : \E v \in {"fired", "skipped"} : Vote(b, v))
    \/ (\E v \in {"allow", "deny"} : Authorize(v))
    \/ DecideJoin
    \/ Dispatch
    \/ Unknown
    \/ Confirm
    \/ Publish

Spec == Init /\ [][Next]_vars

TypeOK ==
    /\ votes \in [Branches -> {"pending", "fired", "skipped"}]
    /\ policy \in {"unchecked", "allow", "deny"}
    /\ join \in {"waiting", "enabled", "blocked"}
    /\ effect \in {"idle", "inflight", "unknown", "confirmed"}
    /\ attempts \in Nat
    /\ published \in BOOLEAN

FullSettlementBarrier == (join # "waiting") => Settled

NoPrematureJoin ==
    (join = "enabled") => (Settled /\ ThresholdMet /\ policy = "allow")

NoDeniedEffect ==
    (attempts > 0) => (policy = "allow" /\ join = "enabled")

NoBlindWriteRetry == attempts <= 1

NoUncertainPublication ==
    published => (effect = "confirmed" /\ join = "enabled" /\ Settled)

BlockedIsInert ==
    (join = "blocked") => (attempts = 0 /\ ~published)

(*
  Compiler-side guarantee rule: a source occurs in >= n-k+1 distinct
  predecessor guarantee sets iff no winning k-subset can exclude it.
  This is checked over EVERY set of source-bearing and fired branches.
*)
StaticGuaranteeSound ==
    \A source \in SUBSET Branches :
        (Cardinality(source) >= Cardinality(Branches) - Threshold + 1)
        => (\A winners \in SUBSET Branches :
              (Cardinality(winners) >= Threshold)
              => (source \cap winners # {}))

=============================================================================
