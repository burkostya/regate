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
)

// CompensationApproval is the operator's explicit authorization statement.
// Only a digest of the explanation is persisted to minimize sensitive data
// retention; the approval actor is an authenticated-admin assertion, not an
// individually signed identity.
type CompensationApproval struct {
    Actor string `json:"actor"`
    ReasonDigest string `json:"reason_digest"`
    At time.Time `json:"at"`
}

// CompensationID is stable across server restarts and independent of the
// chosen adapter. At most one compensation attempt is admitted per original
// effect, including when that attempt's own outcome is unknown.
func CompensationID(originalEffectID string)string {
    return EffectID("compensation:"+originalEffectID,"inverse","attempt")
}

func validCompensationActor(actor string)bool {
    if actor=="" || len(actor)>256 || strings.TrimSpace(actor)!=actor {return false}
    for _,r:=range actor {if r<' ' || r==127 {return false}}
    return true
}

// BeginCompensation is a transactional authorization AND dispatch barrier.
// Compensation does not rewrite the source effect; it starts a new durable
// effect linked to the original. Only an explicitly resolved-applied source
// with a matching endpoint and original request digest can be compensated.
//
// It is forbidden to compensate a merely "confirmed" response: transport
// success alone does not prove that an application-side write was applied.
func (s *Store) BeginCompensation(originalID,forOperation,targetDigest,operation string,
    originalIntent,compensationArguments any,actor,reason string)(Effect,error) {
    if !validCompensationActor(actor) ||
        strings.TrimSpace(reason)=="" || len(reason)>2048 ||
        forOperation=="" || operation=="" || originalID=="" ||
        len(targetDigest)!=64 {
        return Effect{},ErrInvalidEvidence
    }
    if _,err:=hex.DecodeString(targetDigest);err!=nil{return Effect{},ErrInvalidEvidence}
    inputDigest,err:=DigestIntent(originalIntent)
    if err!=nil{return Effect{},ErrInvalidEvidence}
    inverseDigest,err:=DigestIntent(compensationArguments)
    if err!=nil{return Effect{},ErrInvalidEvidence}
    hash:=sha256.Sum256([]byte(reason))
    approval:=&CompensationApproval{
        Actor:actor,
        ReasonDigest:hex.EncodeToString(hash[:]),
        At:time.Now().UTC(),
    }
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Effect{},err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return Effect{},err}
    var originalJSON string
    if err=tx.QueryRow("SELECT record FROM effect_current WHERE effect_id=?",originalID).Scan(&originalJSON);err!=nil{
        return Effect{},err
    }
    var original Effect
    if err=json.Unmarshal([]byte(originalJSON),&original);err!=nil{return Effect{},err}
    if original.ID!=originalID || original.Compensates!="" ||
        original.Status!="resolved_applied" || original.TargetDigest=="" ||
        original.TargetDigest!=targetDigest || original.Operation!=forOperation ||
        original.IntentDigest=="" || original.IntentDigest!=inputDigest {
        return Effect{},ErrInvalidEvidence
    }
    id:=CompensationID(originalID)
    var priorJSON string
    err=tx.QueryRow("SELECT record FROM effect_current WHERE effect_id=?",id).Scan(&priorJSON)
    if err==nil {
        var previous Effect
        if json.Unmarshal([]byte(priorJSON),&previous)!=nil{return Effect{},fmt.Errorf("corrupt compensation journal")}
        if previous.Compensates!=originalID || previous.Operation!=operation ||
            previous.IntentDigest!=inverseDigest || previous.TargetDigest!=targetDigest {
            return Effect{},ErrEffectConflict
        }
        return Effect{},fmt.Errorf("%w: compensation %s status %s",
            ErrEffectAlreadyAttempted,id,previous.Status)
    }
    if !errors.Is(err,sql.ErrNoRows){return Effect{},err}
    event:=Effect{
        ID:id,RunID:"compensation:"+originalID,Node:"inverse",
        Operation:operation,IntentDigest:inverseDigest,TargetDigest:targetDigest,
        Status:"dispatching",At:approval.At,Compensates:originalID,Approval:approval,
    }
    if err=writeEffectTx(tx,event);err!=nil{return Effect{},err}
    if err=tx.Commit();err!=nil{return Effect{},err}
    return event,nil
}
