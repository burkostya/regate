-------------------------- MODULE EffectLifecycle --------------------------
EXTENDS Naturals, FiniteSets, TLC

(***************************************************************************
 A deliberately small TLA+ sketch of one external effect's lifecycle.       
 It models the key safety requirement: if a request was dispatched and the   
 response was lost, no blind automatic retry is allowed.                    
 This small safety abstraction is checked by TLC in GitHub CI.   
***************************************************************************)
VARIABLES stage, attempts, knownOutcome

vars == <<stage, attempts, knownOutcome>>

Init == /\ stage = "planned"
        /\ attempts = 0
        /\ knownOutcome = FALSE

Dispatch == /\ stage = "planned"
            /\ stage' = "inflight"
            /\ attempts' = attempts + 1
            /\ UNCHANGED knownOutcome

Confirm == /\ stage = "inflight"
           /\ stage' = "confirmed"
           /\ knownOutcome' = TRUE
           /\ UNCHANGED attempts

LoseResponse == /\ stage = "inflight"
                /\ stage' = "unknown"
                /\ UNCHANGED <<attempts, knownOutcome>>

Reconcile == /\ stage = "unknown"
             /\ stage' = "resolved"
             /\ knownOutcome' = TRUE
             /\ UNCHANGED attempts

Next == Dispatch \/ Confirm \/ LoseResponse \/ Reconcile
Spec == Init /\ [][Next]_vars

TypeOK == /\ stage \in {"planned", "inflight", "confirmed", "unknown", "resolved"}
          /\ attempts \in Nat
          /\ knownOutcome \in BOOLEAN

NoBlindRetry == attempts <= 1
ResolvedIsKnown == (stage = "resolved" => knownOutcome)

=============================================================================
