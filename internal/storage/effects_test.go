package storage

import (
    "context"
    "crypto/sha256"
    "encoding/hex"
    "errors"
    "fmt"
    "sync"
    "testing"
)

type fakeReadback struct {
    observation ReadbackObservation
    err error
    called int
}
func (f *fakeReadback) ReadBack(_ context.Context, e Effect)(ReadbackObservation,error) {
    f.called++
    return f.observation,f.err
}
func probeFor(e Effect,verdict string) ReadbackObservation {
    digest:=sha256.Sum256([]byte("resource snapshot for "+e.ID))
    return ReadbackObservation{
        EffectID:e.ID,IntentDigest:e.IntentDigest,Operation:e.Operation,
        Resource:"mcp://crm/customers/C123",Method:"tools/call:find_by_external_id",
        EvidenceDigest:hex.EncodeToString(digest[:]),Verifier:"verified-crm-adapter",
        Verdict:verdict,
    }
}

func TestEffectStableIdentityAndAtomicNoRetry(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    a:=map[string]any{"id":"C123","payload":map[string]any{"a":1,"b":2}}
    b:=map[string]any{"payload":map[string]any{"b":2,"a":1},"id":"C123"}
    key1:=EffectID("run1","create","crm.create")
    key2:=EffectID("run1","create","crm.create")
    if key1!=key2 || key1==EffectID("run2","create","crm.create") ||
        key1==EffectID("run1","create2","crm.create") {
        t.Fatal("effect IDs are not stable and scope-separated")
    }
    h1,_:=DigestIntent(a)
    h2,_:=DigestIntent(b)
    if h1!=h2 {t.Fatal("identical request map produced different intent digests")}
    first,err:=s.BeginEffect("run1","create","crm.create",a)
    if err!=nil || first.ID!=key1 || first.Status!="dispatching" {t.Fatalf("begin effect: %#v %v",first,err)}
    if _,err=s.BeginEffect("run1","create","crm.create",b);!errors.Is(err,ErrEffectAlreadyAttempted) {
        t.Fatalf("double dispatch admitted: %v",err)
    }
    if _,err=s.BeginEffect("run1","create","crm.create",map[string]any{"id":"other"});!errors.Is(err,ErrEffectConflict) {
        t.Fatalf("different payload reused effect identity: %v",err)
    }
    if err=s.CompleteEffect(first.ID,"unknown","connection reset");err!=nil{t.Fatal(err)}
    if err=s.CompleteEffect(first.ID,"confirmed","");!errors.Is(err,ErrEffectAlreadyAttempted){
        t.Fatalf("unknown outcome was overwritten as success: %v",err)
    }
    if _,err=s.BeginEffect("run1","create","crm.create",a);!errors.Is(err,ErrEffectAlreadyAttempted) {
        t.Fatalf("unknown write was redispatched: %v",err)
    }
    if n:=len(s.Effects());n!=1 {t.Fatalf("expected one effect projection, got %d",n)}
    var count int
    if err=s.db.QueryRow("SELECT count(*) FROM effect_events").Scan(&count);err!=nil{t.Fatal(err)}
    if count!=2 {t.Fatalf("unexpected number of durable status transitions: %d",count)}
}
func TestEffectReadbackResolutionRequiresBoundEvidence(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    attempt,err:=s.BeginEffect("run2","write","crm.write",map[string]any{"id":"C2"})
    if err!=nil{t.Fatal(err)}
    if err=s.CompleteEffect(attempt.ID,"unknown","response lost");err!=nil{t.Fatal(err)}
    observation:=probeFor(attempt,"applied")
    verifier:=&fakeReadback{observation:observation}
    invalid:=observation
    invalid.IntentDigest="bad-intent-digest"
    if _,err=s.ResolveWithReadback(context.Background(),attempt.ID,&fakeReadback{observation:invalid});!errors.Is(err,ErrInvalidEvidence){
        t.Fatalf("unbound readback evidence accepted: %v",err)
    }
    pending,err:=s.EffectGet(attempt.ID)
    if err!=nil || pending.Status!="unknown" {t.Fatalf("invalid probe changed journal: %v %#v",err,pending)}
    event,err:=s.ResolveWithReadback(context.Background(),attempt.ID,verifier)
    if err!=nil{t.Fatal(err)}
    if event.Status!="resolved_applied" || event.Resolution==nil ||
        event.Resolution.Kind!="readback" || event.Resolution.EvidenceDigest!=observation.EvidenceDigest ||
        verifier.called!=1 {
        t.Fatalf("verified outcome lost provenance: %#v",event)
    }
    if _,err=s.ResolveWithReadback(context.Background(),attempt.ID,verifier);!errors.Is(err,ErrEffectUnresolved) {
        t.Fatalf("resolved effect was re-verifiable: %v",err)
    }
    if _,err=s.BeginEffect("run2","write","crm.write",map[string]any{"id":"C2"});!errors.Is(err,ErrEffectAlreadyAttempted){
        t.Fatalf("resolved external write became retryable: %v",err)
    }
}
func TestUnverifiedNegativeReadbackCannotUnlockRetry(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    e,err:=s.BeginEffect("run-absence","write","crm.create",map[string]any{"id":"C99"})
    if err!=nil{t.Fatal(err)}
    if err=s.CompleteEffect(e.ID,"unknown","lost response");err!=nil{t.Fatal(err)}
    eventual:=probeFor(e,"absent") // no consistency attestation
    if _,err=s.ResolveWithReadback(context.Background(),e.ID,&fakeReadback{observation:eventual});!errors.Is(err,ErrInvalidEvidence) {
        t.Fatalf("non-authoritative missing record treated as a verified absence: %v",err)
    }
    now,err:=s.EffectGet(e.ID)
    if err!=nil || now.Status!="unknown" {t.Fatalf("negative probe changed unknown state: %#v %v",now,err)}
    eventual.Authoritative=true
    resolved,err:=s.ResolveWithReadback(context.Background(),e.ID,&fakeReadback{observation:eventual})
    if err!=nil || resolved.Status!="resolved_absent" {t.Fatalf("authoritative absence rejected: %#v %v",resolved,err)}
    if _,err=s.BeginEffect("run-absence","write","crm.create",map[string]any{"id":"C99"});!errors.Is(err,ErrEffectAlreadyAttempted) {
        t.Fatalf("resolved absence allowed blind same-run replay: %v",err)
    }
}

func TestOperatorDecisionDistinctFromVerifiedReadback(t *testing.T){
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    event,err:=s.BeginEffect("run3","delete","crm.delete",map[string]any{"id":3})
    if err!=nil{t.Fatal(err)}
    if _,err=s.ResolveByOperator(event.ID,"operator","absent","not dispatched");!errors.Is(err,ErrEffectUnresolved){
        t.Fatalf("resolved before unknown: %v",err)
    }
    if err=s.CompleteEffect(event.ID,"unknown","lost response");err!=nil{t.Fatal(err)}
    if _,err=s.ResolveByOperator(event.ID,"","absent","decided");!errors.Is(err,ErrInvalidEvidence){
        t.Fatalf("anonymous operator approved resolution: %v",err)
    }
    resolved,err:=s.ResolveByOperator(event.ID,"operator-42","absent","verified manually using vendor audit")
    if err!=nil{t.Fatal(err)}
    if resolved.Status!="resolved_absent" || resolved.Resolution==nil ||
        resolved.Resolution.Kind!="operator" || resolved.Resolution.Verifier!="operator-42" {
        t.Fatalf("operator decision misrepresented: %#v",resolved)
    }
    if _,err=s.ResolveByOperator(event.ID,"operator-42","applied","changed mind");!errors.Is(err,ErrEffectUnresolved){
        t.Fatalf("immutable operator decision overwritten: %v",err)
    }
}
func TestCrashKeepsStableUnknownEffectId(t *testing.T) {
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    attempt,err:=first.BeginEffect("recovered-run","make","crm.create",map[string]any{"id":17})
    if err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    restarted,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer restarted.Close()
    current,err:=restarted.EffectGet(attempt.ID)
    if err!=nil || current.Status!="unknown" {
        t.Fatalf("pending write not preserved as unknown: %v %#v",err,current)
    }
    if _,err=restarted.BeginEffect("recovered-run","make","crm.create",map[string]any{"id":17});!errors.Is(err,ErrEffectAlreadyAttempted){
        t.Fatalf("restart permitted duplicate write: %v",err)
    }
}
func TestConcurrentSQLiteEffectAdmissionAcrossConnections(t *testing.T) {
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer first.Close()
    second,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer second.Close()
    stores:=[]*Store{first,second}
    var wg sync.WaitGroup
    results:=make(chan error,2)
    for _,store:=range stores {
        wg.Add(1)
        go func(s *Store){
            defer wg.Done()
            _,err:=s.BeginEffect("shared-run","write","crm.create",map[string]any{"x":"y"})
            results<-err
        }(store)
    }
    wg.Wait()
    close(results)
    succeeded,blocked:=0,0
    for err:=range results{
        switch{
        case err==nil:succeeded++
        case errors.Is(err,ErrEffectAlreadyAttempted):blocked++
        default:t.Fatalf("unexpected concurrency failure: %v",err)
        }
    }
    if succeeded!=1 || blocked!=1 {
        t.Fatalf("double dispatch admitted: succeeded=%d blocked=%d",succeeded,blocked)
    }
    if len(first.Effects())!=1 {t.Fatalf("split-brain effect projection: %v",first.Effects())}
}
func ExampleEffectID(){
    fmt.Println(EffectID("run","node","upstream.tool")==EffectID("run","node","upstream.tool"))
    // Output: true
}
