package storage

import (
    "bytes"
    "context"
    "crypto/rand"
    "encoding/base64"
    "encoding/json"
    "errors"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "testing"
    "time"
)

func testGrantKey(t *testing.T)string {
    t.Helper()
    raw:=make([]byte,32)
    if _,err:=rand.Read(raw);err!=nil{t.Fatal(err)}
    return base64.StdEncoding.EncodeToString(raw)
}
func testGrantEnv(t *testing.T,active string,keys map[string]string) {
    t.Helper()
    raw,err:=json.Marshal(map[string]any{"active":active,"keys":keys})
    if err!=nil{t.Fatal(err)}
    t.Setenv("REGATE_GRANT_KEYS_JSON",string(raw))
}
func testGrantTask(t *testing.T,s *Store)TaskRecord {
    t.Helper()
    active,_,err:=s.Active()
    if err!=nil{t.Fatal(err)}
    task:=TaskRecord{
        TaskID:ID(),Status:"input_required",OwnerDigest:strings.Repeat("a",64),
        Surface:"protected",Tool:"write",Revision:active.ID,
        RequiredScopes:[]string{"surface:use","resource:write"},
        Delegation:&DelegationRequirement{
            Provider:"corp",Issuer:"https://issuer.test",Audience:"https://regate.test/surface",
        },
        CreatedAt:time.Now().UTC(),TTLMS:3600000,PollIntervalMS:1000,
    }
    if err=s.TaskPut(task);err!=nil{t.Fatal(err)}
    return task
}
func newGrantStore(t *testing.T)*Store {
    t.Helper()
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){_ = s.Close()})
    if err=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    return s
}
func expiresGrant()time.Time {return time.Now().Add(4*time.Hour).UTC()}

func TestVaultEncryptsAndBindsToTaskOwnerAndScopes(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    credential:="secret-refresh-token-unique-123456789"
    meta,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,credential,expiresGrant())
    if err!=nil{t.Fatal(err)}
    if meta.Generation!=1 || meta.Provider!="corp" || meta.State!="active" || meta.KeyID!="k1" {
        t.Fatalf("incorrect metadata: %#v",meta)
    }
    metadataJSON,_:=json.Marshal(meta)
    if bytes.Contains(metadataJSON,[]byte(credential)){t.Fatal("secret in public metadata")}
    row,err:=s.db.Query("SELECT nonce,ciphertext FROM task_grants WHERE task_id=?",task.TaskID)
    if err!=nil{t.Fatal(err)}
    var nonce,sealed []byte
    if !row.Next() {t.Fatal("grant not written")}
    if err=row.Scan(&nonce,&sealed);err!=nil{t.Fatal(err)}
    row.Close()
    if len(nonce)!=12 || len(sealed)<24 || bytes.Contains(sealed,[]byte(credential)) {
        t.Fatal("invalid at-rest encryption")
    }
    // A WAL-aware standalone backup must not contain raw credentials either.
    snapshot:=filepath.Join(t.TempDir(),"sealed.sqlite")
    if err=s.Backup(snapshot);err!=nil{t.Fatal(err)}
    content,err:=os.ReadFile(snapshot)
    if err!=nil{t.Fatal(err)}
    if bytes.Contains(content,[]byte(credential)) {t.Fatal("plaintext credential leaked into backup")}
    granted,secret,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || secret!=credential || granted.State!="refreshing" {
        t.Fatalf("decryption or refresh admission failed: %#v %v",granted,err)
    }
    if _,_,err=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);!errors.Is(err,ErrGrantRefreshPending){
        t.Fatalf("refresh admission raced: %v",err)
    }
    if err=s.MarkTaskGrantIndeterminate(context.Background(),granted);err!=nil{t.Fatal(err)}
    state,err:=s.TaskGrantState(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || state.State!="needs_reauth" {t.Fatalf("ambiguous rotation remained active: %#v %v",state,err)}
    if _,_,err=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);!errors.Is(err,ErrGrantRefreshPending){
        t.Fatalf("ambiguous rotation retried: %v",err)
    }
}

func TestVaultWrongOwnerAndSourceBindingFailClosed(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    token:="private-refresh-bound-to-this-task"
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,strings.Repeat("b",64),token,expiresGrant());
        !errors.Is(err,ErrGrantUnavailable){t.Fatalf("unauthorized task got a credential: %v",err)}
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,token,expiresGrant());err!=nil{t.Fatal(err)}
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,token,expiresGrant());
        !errors.Is(err,ErrGrantConflict) {t.Fatalf("replaced unique grant: %v",err)}
    if _,_,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,strings.Repeat("b",64));
        !errors.Is(err,ErrGrantUnavailable){t.Fatalf("wrong owner decrypted secret: %v",err)}
    altered:=task
    altered.RequiredScopes=[]string{"resource:write"} // capability was widened relative to persisted original
    if err:=s.TaskPut(altered);err!=nil{t.Fatal(err)}
    if _,_,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantUnavailable){t.Fatalf("scope drift was authorized: %v",err)}
    altered=task
    altered.Delegation=&DelegationRequirement{Provider:"another-idp",Issuer:task.Delegation.Issuer,Audience:task.Delegation.Audience}
    if err:=s.TaskPut(altered);err!=nil{t.Fatal(err)}
    if _,_,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantUnavailable){t.Fatalf("provider drift was authorized: %v",err)}
}

func TestVaultAuthenticatedEncryptionTamperFailsClosed(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,"sealed-refresh",expiresGrant());err!=nil{t.Fatal(err)}
    if _,err:=s.db.Exec("UPDATE task_grants SET audience=? WHERE task_id=?","https://attacker.test",task.TaskID);err!=nil{t.Fatal(err)}
    if _,_,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantUnavailable){t.Fatalf("modified audience authorized: %v",err)}
    if _,err:=s.db.Exec("UPDATE task_grants SET audience=? WHERE task_id=?",task.Delegation.Audience,task.TaskID);err!=nil{t.Fatal(err)}
    if _,err:=s.db.Exec("UPDATE task_grants SET ciphertext=? WHERE task_id=?",[]byte("not-valid-ciphertext"),task.TaskID);err!=nil{t.Fatal(err)}
    if _,_,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantUnavailable){t.Fatalf("modified ciphertext decrypted: %v",err)}
    state,err:=s.TaskGrantState(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || state.State!="active" {t.Fatalf("failed authenticated decrypt changed state: %#v %v",state,err)}
}

func TestVaultRefreshRotationAndRevocationFencing(t *testing.T) {
    key1,key2:=testGrantKey(t),testGrantKey(t)
    testGrantEnv(t,"k1",map[string]string{"k1":key1,"k2":key2})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,"refresh-old",expiresGrant());err!=nil{t.Fatal(err)}
    stage,secret,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || secret!="refresh-old" {t.Fatalf("refresh stage: %v",err)}
    updated,err:=s.FinishTaskGrantRefresh(context.Background(),stage,"refresh-new",expiresGrant())
    if err!=nil || updated.Generation!=2 || updated.State!="active" {t.Fatalf("rotation failed: %#v %v",updated,err)}
    if _,err=s.FinishTaskGrantRefresh(context.Background(),stage,"late-token",expiresGrant());!errors.Is(err,ErrGrantConflict) {
        t.Fatalf("stale refresh completion overwrote rotated token: %v",err)
    }
    // Rewrap with the new active encryption key; old key can then be retired.
    s.grantKeys.active="k2"
    rewrapped,err:=s.RewrapTaskGrant(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || rewrapped.KeyID!="k2" || rewrapped.Generation!=3 {
        t.Fatalf("key rotation failed: %#v %v",rewrapped,err)
    }
    delete(s.grantKeys.keys,"k1")
    reserved,secret,err:=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || secret!="refresh-new"{t.Fatalf("old key required after rewrap: %v",err)}
    if err=s.RevokeTaskGrant(context.Background(),task.TaskID,task.OwnerDigest);err!=nil{t.Fatal(err)}
    if _,err=s.FinishTaskGrantRefresh(context.Background(),reserved,"should-not-restore",expiresGrant());
        !errors.Is(err,ErrGrantConflict){t.Fatalf("revocation lost CAS: %v",err)}
    if _,_,err=s.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantRefreshPending){t.Fatalf("revoked grant was usable: %v",err)}
    var length int
    if err=s.db.QueryRow("SELECT length(ciphertext) FROM task_grants WHERE task_id=?",task.TaskID).Scan(&length);
        err!=nil || length!=0 {t.Fatalf("revoked ciphertext not cleared: %d %v",length,err)}
}
func TestVaultInterruptedRefreshReopensAsNeedsReauthorization(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=first.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    task:=testGrantTask(t,first)
    if _,err=first.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,"before-crash",expiresGrant());err!=nil{t.Fatal(err)}
    meta,_,err:=first.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    next,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer next.Close()
    if err=next.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    restored,err:=next.TaskGrantState(context.Background(),task.TaskID,task.OwnerDigest)
    if err!=nil || restored.State!="needs_reauth" || restored.Generation<=meta.Generation {
        t.Fatalf("crashed OAuth refresh was retried: %#v %v",restored,err)
    }
    if _,_,err=next.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantRefreshPending){t.Fatalf("lost remote token rotation repeated: %v",err)}
}
func TestVaultMissingKeyNeverResurrectsCredentials(t *testing.T){
    key:=testGrantKey(t)
    testGrantEnv(t,"k1",map[string]string{"k1":key})
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    if err=first.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    task:=testGrantTask(t,first)
    if _,err=first.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,"nonplaintext-value",expiresGrant());err!=nil{t.Fatal(err)}
    if err=first.Close();err!=nil{t.Fatal(err)}
    t.Setenv(grantKeyringEnv,"")
    reopened,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer reopened.Close()
    if err=reopened.AcquireExecutionLease();err!=nil{t.Fatal(err)}
    if _,_,err=reopened.BeginTaskGrantRefresh(context.Background(),task.TaskID,task.OwnerDigest);
        !errors.Is(err,ErrGrantKeyUnavailable){t.Fatalf("opened grant without key: %v",err)}
}
func TestVaultV2MigrationPreservesExistingTasks(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    dir:=t.TempDir()
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    task:=testGrantTask(t,s)
    if _,err=s.db.Exec("DROP TABLE task_grants");err!=nil{t.Fatal(err)}
    if _,err=s.db.Exec("PRAGMA user_version=2");err!=nil{t.Fatal(err)}
    if err=s.Close();err!=nil{t.Fatal(err)}
    up,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer up.Close()
    var version int
    if err=up.db.QueryRow("PRAGMA user_version").Scan(&version);err!=nil{t.Fatal(err)}
    if version!=5 {t.Fatalf("v2 did not migrate to v5: %d",version)}
    if _,err=up.TaskGet(task.TaskID);err!=nil{t.Fatalf("task lost during v3 migration: %v",err)}
}
func TestVaultConcurrentGrantAdmissionSingleWinner(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    var wg sync.WaitGroup
    success:=make(chan struct{},16)
    errs:=make(chan error,16)
    for i:=0;i<16;i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,"credential-rotate",expiresGrant())
            if err!=nil {errs<-err} else {success<-struct{}{}}
        }()
    }
    wg.Wait()
    close(success);close(errs)
    if len(success)!=1{t.Fatalf("expected one grant row, got %d",len(success))}
    for err:=range errs {if !errors.Is(err,ErrGrantConflict){t.Fatalf("unexpected admission error: %v",err)}}
}
func TestGrantKeyConfigRejectsMalformedKeys(t *testing.T){
    cases:=[]string{
        "not-json",
        `{"active":"k1","keys":{"k1":"c2hvcnQ="}}`,
        `{"active":"missing","keys":{"k1":"c2hvcnQ="}}`,
        `{"active":"bad/key","keys":{"bad/key":"c2hvcnQ="}}`,
    }
    for _,raw:=range cases {
        t.Run(raw,func(t *testing.T){
            t.Setenv(grantKeyringEnv,raw)
            if _,err:=LoadGrantKeyRing();!errors.Is(err,ErrGrantKeyUnavailable){
                t.Fatalf("unsafe grant key accepted: %v",err)
            }
        })
    }
}
// Guard the API contract: no public task response can contain sealed grants.
func TestGrantDoesNotAppearInPublicTaskResponse(t *testing.T){
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=newGrantStore(t)
    task:=testGrantTask(t,s)
    if _,err:=s.CreateTaskGrant(context.Background(),task.TaskID,task.OwnerDigest,"refresh-secret",expiresGrant());err!=nil{t.Fatal(err)}
    result,err:=s.TaskGet(task.TaskID)
    if err!=nil{t.Fatal(err)}
    raw,_:=json.Marshal(result.Public())
    if bytes.Contains(raw,[]byte("grant")) || bytes.Contains(raw,[]byte("secret")) {
        t.Fatalf("grant data exposed in public task: %s",raw)
    }
}
