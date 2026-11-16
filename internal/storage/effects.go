package storage

import (
    "context"
    "crypto/sha256"
    "database/sql"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "strings"
    "time"

    "github.com/burkostya/regate/internal/expr"
)

var ErrEffectAlreadyAttempted = errors.New("effect already dispatched or resolved; no automatic replay")
var ErrEffectConflict = errors.New("effect identity reused with a different request")
var ErrEffectUnresolved = errors.New("effect is not awaiting reconciliation")
var ErrInvalidEvidence = errors.New("effect read-back evidence could not be verified")

// EffectID is independent of the number of execution attempts. A second
// dispatch from the same logical run/node/upstream-tool obtains the SAME ID,
// even after a crash or task resume. It cannot create another external write
// by picking a fresh random UUID.
//
// Use domain-separated and length-delimited strings to avoid concatenation
// ambiguity (e.g. run("ab")/node("c") != run("a")/node("bc")).
func EffectID(runID,node,operation string) string {
    h:=sha256.New()
    h.Write([]byte("regate/effect/v1"))
    for _,item:=range []string{runID,node,operation} {
        var n [8]byte
        v:=uint64(len(item))
        for i:=7;i>=0;i-- {n[i]=byte(v);v>>=8}
        h.Write(n[:])
        h.Write([]byte(item))
    }
    return hex.EncodeToString(h.Sum(nil))
}
// DigestIntent is a content fingerprint, NOT the raw arguments. Go's JSON
// encoder produces deterministic key order for maps. An unsupported payload
// is rejected before a write can be journaled or dispatched.
func DigestIntent(arguments any)(string,error) {
    raw,err:=json.Marshal(arguments)
    if err!=nil{return "",err}
    sum:=sha256.Sum256(raw)
    return hex.EncodeToString(sum[:]),nil
}

func (s *Store) EffectGet(id string)(Effect,error) {
    if id==""{return Effect{},fmt.Errorf("empty effect ID")}
    var raw string
    err:=s.db.QueryRow("SELECT record FROM effect_current WHERE effect_id=?",id).Scan(&raw)
    if err!=nil{return Effect{},err}
    var event Effect
    if err=json.Unmarshal([]byte(raw),&event);err!=nil{return Effect{},err}
    return event,nil
}

// BeginEffect is an atomic write admission barrier. It must be committed
// BEFORE an upstream request is sent. Any existing record with the same
// stable ID blocks another dispatch, even when its outcome is unknown.
//
// This intentionally prefers a false "unknown" to duplicate writes after a
// crash between journaling and actually sending the network packet.
// TargetDigest binds evidence to the actual remote endpoint, not merely a
// human-readable upstream alias. It is domain-separated and never stores URLs.
func TargetDigest(transport,endpoint string)string {
    h:=sha256.Sum256([]byte(transport+"\x00"+endpoint))
    return hex.EncodeToString(h[:])
}

func (s *Store) BeginEffect(runID,node,operation string,arguments any)(Effect,error) {
    return s.BeginEffectBound(runID,node,operation,"",arguments)
}

// BeginEffectBound is the admission barrier for resource-bound MCP/HTTP writes.
// Legacy entries with no target fingerprint remain blocked from automatic
// replay, but cannot be automatically reconciled with a new bound adapter.
func (s *Store) BeginEffectBound(runID,node,operation,targetDigest string,arguments any)(Effect,error) {
    if targetDigest!="" {
        if len(targetDigest)!=64{return Effect{},ErrInvalidEvidence}
        if _,err:=hex.DecodeString(targetDigest);err!=nil{return Effect{},ErrInvalidEvidence}
    }
    if runID=="" || node=="" || operation=="" {return Effect{},fmt.Errorf("effect identity incomplete")}
    digest,err:=DigestIntent(arguments)
    if err!=nil{return Effect{},err}
    id:=EffectID(runID,node,operation)
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Effect{},err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return Effect{},err}
    var stored string
    err=tx.QueryRow("SELECT record FROM effect_current WHERE effect_id=?",id).Scan(&stored)
    if err==nil {
        var old Effect
        if json.Unmarshal([]byte(stored),&old)!=nil{return Effect{},fmt.Errorf("corrupt effect journal")}
        if (old.IntentDigest!="" && old.IntentDigest!=digest) || old.TargetDigest!=targetDigest {
            return Effect{},fmt.Errorf("%w: run %s node %s",ErrEffectConflict,runID,node)
        }
        return Effect{},fmt.Errorf("%w: %s status %s",ErrEffectAlreadyAttempted,id,old.Status)
    }
    if !errors.Is(err,sql.ErrNoRows){return Effect{},err}
    e:=Effect{ID:id,RunID:runID,Node:node,Operation:operation,
        IntentDigest:digest,TargetDigest:targetDigest,Status:"dispatching",At:time.Now().UTC()}
    if err=writeEffectTx(tx,e);err!=nil{return Effect{},err}
    if err=tx.Commit();err!=nil{return Effect{},err}
    return e,nil
}

// CompleteEffect is only valid after a matching dispatching event. A lost
// network response transitions to "unknown"; it can NEVER be treated as
// confirmed or absent simply because the connection failed.
func (s *Store) CompleteEffect(id,status,reason string)error {
    if status!="confirmed" && status!="unknown" {return fmt.Errorf("invalid effect completion state %q",status)}
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return err}
    defer tx.Rollback()
    var raw string
    if err=tx.QueryRow("SELECT record FROM effect_current WHERE effect_id=?",id).Scan(&raw);err!=nil{return err}
    var current Effect
    if err=json.Unmarshal([]byte(raw),&current);err!=nil{return err}
    if current.Status!="dispatching" {return fmt.Errorf("%w: current status %s",ErrEffectAlreadyAttempted,current.Status)}
    current.Status=status
    current.Reason=reason
    current.At=time.Now().UTC()
    if err=writeEffectTx(tx,current);err!=nil{return err}
    return tx.Commit()
}

// EffectResolution documents the provenance of a manual decision or a
// verified, resource-scoped read-back. No payloads or credentials are stored
// in the effect journal; only a digest of external evidence.
type EffectResolution struct {
    Kind string `json:"kind"` // "readback" or "operator"
    Verdict string `json:"verdict"` // "applied" or "absent"
    Verifier string `json:"verifier"`
    Resource string `json:"resource,omitempty"`
    Method string `json:"method,omitempty"`
    EvidenceDigest string `json:"evidence_digest,omitempty"`
    Reason string `json:"reason,omitempty"`
    At time.Time `json:"at"`
}

// ReadbackObservation is returned by a trusted, resource-specific adapter
// after it has performed an actual read. The transport may be MCP, HTTP or
// another verified resource adapter; Regate never assumes Jira semantics.
type ReadbackObservation struct {
    EffectID string
    IntentDigest string
    Operation string
    Resource string
    Method string
    EvidenceDigest string
    Verifier string
    Verdict string
    // A negative lookup may be used only when the adapter certifies it
    // represents authoritative, consistency-safe absence. Ordinary
    // eventually-consistent "not found" responses remain UNKNOWN.
    Authoritative bool
}
type ReadbackVerifier interface {
    ReadBack(ctx context.Context,attempt Effect)(ReadbackObservation,error)
}

// ResolveWithReadback doesn't hold a database lock during external I/O.
// After the probe completes, the original effect state is checked again
// atomically; a concurrent operator decision cannot be silently overwritten.
func (s *Store) ResolveWithReadback(ctx context.Context,id string,verifier ReadbackVerifier)(Effect,error) {
    if verifier==nil {return Effect{},ErrInvalidEvidence}
    current,err:=s.EffectGet(id)
    if err!=nil{return Effect{},err}
    if current.Status!="unknown" {return Effect{},ErrEffectUnresolved}
    if current.IntentDigest=="" {
        // Old journal events lack an authenticated argument fingerprint;
        // only an explicit operator can assess their external outcome.
        return Effect{},ErrInvalidEvidence
    }
    observed,err:=verifier.ReadBack(ctx,current)
    if err!=nil{return Effect{},err}
    if observed.EffectID!=current.ID || observed.IntentDigest!=current.IntentDigest ||
        observed.Operation!=current.Operation || observed.Resource=="" ||
        observed.Method=="" || observed.Verifier=="" ||
        len(observed.EvidenceDigest)!=64 ||
        (observed.Verdict!="applied" && observed.Verdict!="absent") ||
        (observed.Verdict=="absent" && !observed.Authoritative) {
        return Effect{},ErrInvalidEvidence
    }
    if _,err=hex.DecodeString(observed.EvidenceDigest);err!=nil{return Effect{},ErrInvalidEvidence}
    evidence:=EffectResolution{
        Kind:"readback",Verdict:observed.Verdict,Verifier:observed.Verifier,
        Resource:observed.Resource,Method:observed.Method,
        EvidenceDigest:observed.EvidenceDigest,At:time.Now().UTC(),
    }
    return s.recordResolution(current,evidence)
}
// ResolveByOperator records a deliberate human decision. An explicit
// operator's reasoning is not misrepresented as a verified external read.
func (s *Store) ResolveByOperator(id,actor,verdict,reason string)(Effect,error) {
    if strings.TrimSpace(actor)=="" || strings.TrimSpace(reason)=="" ||
        (verdict!="applied" && verdict!="absent") {
        return Effect{},ErrInvalidEvidence
    }
    current,err:=s.EffectGet(id)
    if err!=nil{return Effect{},err}
    evidence:=EffectResolution{
        Kind:"operator",Verdict:verdict,Verifier:actor,
        Reason:expr.RedactDiagnostic(reason),At:time.Now().UTC(),
    }
    return s.recordResolution(current,evidence)
}
func (s *Store) recordResolution(prior Effect,evidence EffectResolution)(Effect,error) {
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Effect{},err}
    defer tx.Rollback()
    var raw string
    if err=tx.QueryRow("SELECT record FROM effect_current WHERE effect_id=?",prior.ID).Scan(&raw);err!=nil{return Effect{},err}
    var existing Effect
    if err=json.Unmarshal([]byte(raw),&existing);err!=nil{return Effect{},err}
    if existing.Status!="unknown" || existing.IntentDigest!=prior.IntentDigest {
        return Effect{},ErrEffectUnresolved
    }
    existing.Status="resolved_"+evidence.Verdict
    existing.Resolution=&evidence
    existing.Reason="" // Do not silently inherit a credential-bearing transport error.
    existing.At=time.Now().UTC()
    if err=writeEffectTx(tx,existing);err!=nil{return Effect{},err}
    if err=tx.Commit();err!=nil{return Effect{},err}
    return existing,nil
}
