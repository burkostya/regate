package engine

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "reflect"
    "strconv"
    "strings"

    "github.com/burkostya/regate/internal/config"
    "github.com/burkostya/regate/internal/mcp"
    "github.com/burkostya/regate/internal/storage"
)

// MCPReadback is a positive-only verifier for a single HCL-declared
// read-only lookup. Its expected identity is recovered from operator-supplied
// original write arguments, but must match the durable SHA-256 intent digest.
// The endpoint fingerprint must match the endpoint that received the write.
type MCPReadback struct {
    Definition config.Readback
    Upstream config.Upstream
    Conn *mcp.Conn
    Intent map[string]any
}

func (v *MCPReadback) ReadBack(ctx context.Context,effect storage.Effect)(storage.ReadbackObservation,error) {
    expectedTarget:=storage.TargetDigest("mcp",v.Upstream.URL)
    if effect.Operation!=v.Definition.WriteOperation ||
        effect.TargetDigest=="" || effect.TargetDigest!=expectedTarget ||
        v.Definition.Upstream!=v.Upstream.Name || !v.Definition.SafeRead {
        return storage.ReadbackObservation{},storage.ErrInvalidEvidence
    }
    digest,err:=storage.DigestIntent(v.Intent)
    if err!=nil || digest!=effect.IntentDigest {
        return storage.ReadbackObservation{},storage.ErrInvalidEvidence
    }
    expected,ok:=jsonPointer(v.Intent,v.Definition.IntentPointer)
    if !ok || !readbackIdentity(expected) {
        return storage.ReadbackObservation{},storage.ErrInvalidEvidence
    }
    response,err:=v.Conn.CallTool(ctx,v.Definition.ReadTool,
        map[string]any{v.Definition.ReadArgument:expected})
    if err!=nil{return storage.ReadbackObservation{},err}
    if response==nil || response["isError"]==true {
        return storage.ReadbackObservation{},storage.ErrInvalidEvidence
    }
    actual,ok:=jsonPointer(response,v.Definition.ResultPointer)
    // Missing or mismatching records are NOT an authoritative proof that the
    // write did not happen. Keep UNKNOWN; never set an absent verdict here.
    if !ok || !reflect.DeepEqual(expected,actual) {
        return storage.ReadbackObservation{},storage.ErrInvalidEvidence
    }
    // A provider may already have a different record with the same ID.
    // Identity alone does not establish that the unknown write's payload
    // was applied. Opted-in semantic comparison refuses such false proof.
    if (v.Definition.MatchIntentPointer=="") != (v.Definition.MatchResultPointer=="") {
        return storage.ReadbackObservation{},storage.ErrInvalidEvidence
    }
    if v.Definition.MatchIntentPointer!="" {
        wanted,found:=jsonPointer(v.Intent,v.Definition.MatchIntentPointer)
        if !found {return storage.ReadbackObservation{},storage.ErrInvalidEvidence}
        observed,found:=jsonPointer(response,v.Definition.MatchResultPointer)
        if !found || !reflect.DeepEqual(wanted,observed) {
            return storage.ReadbackObservation{},storage.ErrInvalidEvidence
        }
    }
    raw,err:=json.Marshal(response)
    if err!=nil{return storage.ReadbackObservation{},storage.ErrInvalidEvidence}
    evidence:=sha256.Sum256(raw)
    return storage.ReadbackObservation{
        EffectID:effect.ID,IntentDigest:effect.IntentDigest,
        Operation:effect.Operation,
        Resource:fmt.Sprintf("mcp://%s/endpoint/%s",v.Upstream.Name,expectedTarget),
        Method:"tools/call:"+v.Definition.ReadTool,
        EvidenceDigest:hex.EncodeToString(evidence[:]),
        Verifier:"hcl.readback:"+v.Definition.Name,
        Verdict:"applied",
    },nil
}

// ReconcileMCP proves a positive match through an actual remote MCP tool
// read. Without a matching source endpoint and original intent, it refuses
// to send even the read-only lookup. Operator decisions remain distinct.
func (r *Runtime) ReconcileMCP(ctx context.Context,cfg *config.Config,
    effectID,adapter string,intent map[string]any)(storage.Effect,error) {
    if r.Store==nil {return storage.Effect{},fmt.Errorf("durable store required")}
    if err:=r.Store.RequireExecutionLeader(ctx);err!=nil{return storage.Effect{},err}
    rule,ok:=cfg.Readbacks[adapter]
    if !ok{return storage.Effect{},fmt.Errorf("unknown readback adapter")}
    u,ok:=cfg.Upstreams[rule.Upstream]
    if !ok || !rule.SafeRead {return storage.Effect{},storage.ErrInvalidEvidence}
    effect,err:=r.Store.EffectGet(effectID)
    if err!=nil{return storage.Effect{},err}
    if effect.Status!="unknown" {return storage.Effect{},storage.ErrEffectUnresolved}
    if effect.Operation!=rule.WriteOperation || effect.TargetDigest=="" ||
        effect.TargetDigest!=storage.TargetDigest("mcp",u.URL) {
        return storage.Effect{},storage.ErrInvalidEvidence
    }
    digest,err:=storage.DigestIntent(intent)
    if err!=nil || digest!=effect.IntentDigest {return storage.Effect{},storage.ErrInvalidEvidence}
    if err=checkOutbound(u.URL);err!=nil{return storage.Effect{},err}
    conn,err:=r.connFor(ctx,cfg,u)
    if err!=nil{return storage.Effect{},err}
    verifier:=&MCPReadback{Definition:rule,Upstream:u,Conn:conn,Intent:intent}
    return r.Store.ResolveWithReadback(ctx,effectID,verifier)
}

func readbackIdentity(value any)bool {
    switch v:=value.(type) {
    case string:
        return v!=""
    case float64,int64,int,uint64,uint:
        return true
    default:
        return false
    }
}

// jsonPointer implements the escaped-key and array-index parts of RFC 6901.
// It operates only on values already decoded from JSON; no expressions,
// network paths or user-supplied code are evaluated.
func jsonPointer(root any,pointer string)(any,bool) {
    if len(pointer)<2 || pointer[0]!='/' {return nil,false}
    value:=root
    for _,segment:=range strings.Split(pointer[1:],"/") {
        segment=strings.ReplaceAll(strings.ReplaceAll(segment,"~1","/"),"~0","~")
        switch current:=value.(type) {
        case map[string]any:
            var ok bool
            value,ok=current[segment]
            if !ok{return nil,false}
        case []any:
            index,err:=strconv.Atoi(segment)
            if err!=nil || index<0 || strconv.Itoa(index)!=segment || index>=len(current) {
                return nil,false
            }
            value=current[index]
        default:
            return nil,false
        }
    }
    return value,true
}
