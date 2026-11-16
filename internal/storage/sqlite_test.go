package storage

import (
    "database/sql"
    "encoding/json"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"
)
func initSource()string{return `regate {listen="127.0.0.1:8080"}`}

func TestSQLiteRevisionDraftRollbackAndConflict(t *testing.T) {
    dir:=t.TempDir()
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){s.Close()})
    first,source,err:=s.Active()
    if err!=nil || source!=initSource() {t.Fatalf("first revision: %v %q",err,source)}
    d1,err:=s.DraftCreate()
    if err!=nil{t.Fatal(err)}
    d2,err:=s.DraftCreate()
    if err!=nil{t.Fatal(err)}
    source2:=source+"\n# deliberately changed\n"
    d1,err=s.DraftUpdate(d1.ID,source2)
    if err!=nil || d1.Source!=source2{t.Fatalf("draft patch: %#v %v",d1,err)}
    r2,err:=s.Activate(d1.ID)
    if err!=nil || r2.Parent!=first.ID{t.Fatalf("activation: %#v %v",r2,err)}
    if _,err=s.Activate(d2.ID);err==nil || !strings.Contains(err.Error(),"revision conflict"){
        t.Fatalf("stale draft committed: %v",err)
    }
    r3,err:=s.Rollback(first.ID)
    if err!=nil || r3.Parent!=r2.ID || r3.ID==first.ID {t.Fatalf("rollback: %#v %v",r3,err)}
    active,got,err:=s.Active()
    if err!=nil || active.ID!=r3.ID || got!=source {t.Fatalf("active rollback: %v %#v %q",err,active,got)}
    if len(s.History())!=3 {t.Fatalf("missing history: %#v",s.History())}
    if err=s.Close();err!=nil{t.Fatal(err)}
    restarted,err:=New(dir,"different initial source must be ignored")
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){restarted.Close()})
    r,_,err:=restarted.Active()
    if err!=nil || r.ID!=r3.ID {t.Fatalf("active revision lost: %#v %v",r,err)}
    if _,err=restarted.DraftGet(d2.ID);err!=nil{t.Fatalf("conflicted draft lost: %v",err)}
    if b,_:=os.ReadFile(filepath.Join(dir,databaseName));len(b)<16 || string(b[:16])!="SQLite format 3\x00"{
        t.Fatal("database is not a SQLite file")
    }
}
func TestSQLiteDurableEffectsTaskCheckpointsAndBackup(t *testing.T) {
    dir:=t.TempDir()
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    r,_,_:=s.Active()
    trace:=Trace{RunID:ID(),Revision:r.ID,Status:"success",At:time.Now().UTC()}
    s.WriteTrace(trace)
    pending:=Effect{ID:ID(),RunID:trace.RunID,Node:"create",Operation:"crm.create",Status:"dispatching"}
    if err=s.Effect(pending);err!=nil{t.Fatal(err)}
    settled:=Effect{ID:ID(),RunID:trace.RunID,Node:"read",Operation:"crm.read",Status:"confirmed"}
    if err=s.Effect(settled);err!=nil{t.Fatal(err)}
    task:=TaskRecord{TaskID:ID(),Status:"working",OwnerDigest:"ownerhash",Surface:"private",
        Revision:r.ID,TTLMS:3600000,PollIntervalMS:1000}
    if err=s.TaskPut(task);err!=nil{t.Fatal(err)}
    cp:=map[string]any{"run_id":ID(),"node":"suspend","value":map[string]any{"x":true}}
    checkpointID:=cp["run_id"].(string)
    if err=s.PutSuspended(checkpointID,cp);err!=nil{t.Fatal(err)}
    if err=s.SaveLayout("graph_1",map[string]any{"nodes":[]any{map[string]any{"x":1}}});err!=nil{t.Fatal(err)}
    if err=s.Close();err!=nil{t.Fatal(err)}
    restarted,err:=New(dir,"ignored")
    if err!=nil{t.Fatal(err)}
    t.Cleanup(func(){restarted.Close()})
    effects:=restarted.Effects()
    if len(effects)!=2{t.Fatalf("missing effects: %#v",effects)}
    statuses:=map[string]string{}
    for _,e:=range effects{statuses[e.ID]=e.Status}
    if statuses[pending.ID]!="unknown" || statuses[settled.ID]!="confirmed"{
        t.Fatalf("lost uncertain/confirmed effect: %#v",effects)
    }
    item,err:=restarted.TaskGet(task.TaskID)
    if err!=nil || item.Status!="failed" || item.Error==nil{
        t.Fatalf("interrupted task incorrectly resumed: %#v %v",item,err)
    }
    if len(restarted.Traces())!=1{t.Fatal("durable trace lost")}
    var retrieved map[string]any
    if err=restarted.GetSuspended(checkpointID,&retrieved);err!=nil{t.Fatal(err)}
    if retrieved["node"]!="suspend"{t.Fatalf("checkpoint damaged: %v",retrieved)}
    layout,err:=restarted.Layout("graph_1")
    if err!=nil || len(layout)==0{t.Fatalf("layout lost: %v %v",layout,err)}
    backup:=filepath.Join(t.TempDir(),"snapshot.sqlite")
    if err=restarted.Backup(backup);err!=nil{t.Fatal(err)}
    if err=restarted.Backup(backup);err==nil{t.Fatal("backup silently overwritten")}
    copied,err:=sql.Open("sqlite3","file:"+backup)
    if err!=nil{t.Fatal(err)}
    defer copied.Close()
    var count int
    if err=copied.QueryRow("SELECT count(*) FROM effect_current").Scan(&count);err!=nil || count!=2 {
        t.Fatalf("snapshot missing event projection: %d %v",count,err)
    }
    if err=restarted.Close();err!=nil{t.Fatal(err)}
    again,err:=New(dir,"ignored")
    if err!=nil{t.Fatal(err)}
    defer again.Close()
    var eventsCount int
    if err=again.db.QueryRow("SELECT count(*) FROM effect_events").Scan(&eventsCount);err!=nil{
        t.Fatal(err)
    }
    if eventsCount!=3 {t.Fatalf("unknown effect was re-journaled at every restart: %d",eventsCount)}
}

func createLegacyFixture(t *testing.T,dir string) (Revision,Draft,TaskRecord,string){
    t.Helper()
    for _,name:=range []string{"revisions","drafts","layouts","suspended","tasks"}{
        if err:=os.MkdirAll(filepath.Join(dir,name),0700);err!=nil{t.Fatal(err)}
    }
    source:=initSource()
    revision:=Revision{ID:Hash(source+"import"),Digest:Hash(source),Activated:time.Now().UTC()}
    rdata,_:=json.Marshal([]Revision{revision})
    if err:=os.WriteFile(filepath.Join(dir,"history.json"),rdata,0600);err!=nil{t.Fatal(err)}
    if err:=os.WriteFile(filepath.Join(dir,"revisions",revision.ID+".hcl"),[]byte(source),0600);err!=nil{t.Fatal(err)}
    d:=Draft{ID:ID(),Base:revision.ID,Source:source+"\n# pending edit\n",Created:time.Now().UTC()}
    ddata,_:=json.Marshal(d)
    if err:=os.WriteFile(filepath.Join(dir,"drafts",d.ID+".json"),ddata,0600);err!=nil{t.Fatal(err)}
    task:=TaskRecord{TaskID:ID(),OwnerDigest:"user",Revision:revision.ID,Surface:"support",
        Status:"working",CreatedAt:time.Now().UTC(),TTLMS:3600000,PollIntervalMS:1000}
    b,_:=json.Marshal(task)
    if err:=os.WriteFile(filepath.Join(dir,"tasks",task.TaskID+".json"),b,0600);err!=nil{t.Fatal(err)}
    run:=ID()
    if err:=os.WriteFile(filepath.Join(dir,"suspended",run+".json"),[]byte(`{"checkpoint":"ok"}`),0600);err!=nil{t.Fatal(err)}
    if err:=os.WriteFile(filepath.Join(dir,"layouts","main.json"),[]byte(`{"pos":1}`),0600);err!=nil{t.Fatal(err)}
    trace,_:=json.Marshal(Trace{RunID:run,Revision:revision.ID,Status:"done"})
    if err:=os.WriteFile(filepath.Join(dir,"traces.jsonl"),append(trace,'\n'),0600);err!=nil{t.Fatal(err)}
    event,_:=json.Marshal(Effect{ID:ID(),RunID:run,Status:"dispatching",Operation:"crm.write"})
    if err:=os.WriteFile(filepath.Join(dir,"effects.jsonl"),append(event,'\n'),0600);err!=nil{t.Fatal(err)}
    return revision,d,task,run
}
func TestLegacyImportAtomicPreservesFilesAndAllDomains(t *testing.T) {
    dir:=t.TempDir()
    old,draft,task,run:=createLegacyFixture(t,dir)
    original,err:=os.ReadFile(filepath.Join(dir,"history.json"))
    if err!=nil{t.Fatal(err)}
    s,err:=New(dir,"new initial must not replace legacy data")
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    active,source,err:=s.Active()
    if err!=nil || active.ID!=old.ID || source!=initSource(){t.Fatalf("legacy revision lost: %#v %q %v",active,source,err)}
    migrated,err:=s.DraftGet(draft.ID)
    if err!=nil || migrated.Source!=draft.Source {t.Fatalf("draft lost: %#v %v",migrated,err)}
    task2,err:=s.TaskGet(task.TaskID)
    if err!=nil || task2.Status!="failed"{t.Fatalf("old running task resumed: %#v %v",task2,err)}
    var checkpoint map[string]any
    if err=s.GetSuspended(run,&checkpoint);err!=nil || checkpoint["checkpoint"]!="ok"{t.Fatalf("checkpoint lost: %v %v",checkpoint,err)}
    if got,err:=s.Layout("main");err!=nil || got["pos"]!=float64(1) {t.Fatalf("layout lost: %v %v",got,err)}
    if len(s.Traces())!=1 || len(s.Effects())!=1 || s.Effects()[0].Status!="unknown"{
        t.Fatalf("effect/trace recovery incorrect: effects=%v traces=%v",s.Effects(),s.Traces())
    }
    after,err:=os.ReadFile(filepath.Join(dir,"history.json"))
    if err!=nil || string(after)!=string(original){t.Fatal("legacy backup mutated")}
}
func TestLegacyImportCorruptionRollsBackEntireTransaction(t *testing.T){
    dir:=t.TempDir()
    old,draft,_,_:=createLegacyFixture(t,dir)
    path:=filepath.Join(dir,"revisions",old.ID+".hcl")
    if err:=os.WriteFile(path,[]byte("damaged source"),0600);err!=nil{t.Fatal(err)}
    if _,err:=New(dir,initSource());err==nil || !strings.Contains(err.Error(),"digest mismatch"){
        t.Fatalf("bad legacy state accepted: %v",err)
    }
    // The partially created DB schema must contain no imported revision.
    b,err:=sql.Open("sqlite3","file:"+filepath.Join(dir,databaseName))
    if err!=nil{t.Fatal(err)}
    var count int
    if err=b.QueryRow("SELECT count(*) FROM revisions").Scan(&count);err!=nil && !strings.Contains(err.Error(),"no such table"){
        t.Fatal(err)
    }
    b.Close()
    if count!=0 {t.Fatalf("partial legacy import committed %d revisions",count)}
    if err=os.WriteFile(path,[]byte(initSource()),0600);err!=nil{t.Fatal(err)}
    s,err:=New(dir,initSource())
    if err!=nil{t.Fatalf("corrected migration failed: %v",err)}
    defer s.Close()
    if _,err=s.DraftGet(draft.ID);err!=nil{t.Fatal(err)}
}
func TestCompetingSQLiteWritersDetectRevisionConflict(t *testing.T){
    dir:=t.TempDir()
    first,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer first.Close()
    second,err:=New(dir,initSource())
    if err!=nil{t.Fatal(err)}
    defer second.Close()
    one,err:=first.DraftCreate()
    if err!=nil{t.Fatal(err)}
    two,err:=second.DraftCreate()
    if err!=nil{t.Fatal(err)}
    if _,err=second.Activate(two.ID);err!=nil{t.Fatal(err)}
    if _,err=first.Activate(one.ID);err==nil || !strings.Contains(err.Error(),"revision conflict"){
        t.Fatalf("multi-connection stale draft activated: %v",err)
    }
    if len(first.History())!=2 || len(second.History())!=2 {
        t.Fatalf("history split across connections: %v %v",first.History(),second.History())
    }
}
func TestMalformedLegacyJournalRequiresOperatorResolution(t *testing.T){
    dir:=t.TempDir()
    _,_,_,_ = createLegacyFixture(t,dir)
    file:=filepath.Join(dir,"effects.jsonl")
    f,err:=os.OpenFile(file,os.O_APPEND|os.O_WRONLY,0600)
    if err!=nil{t.Fatal(err)}
    if _,err=f.WriteString("{partial");err!=nil{t.Fatal(err)}
    f.Close()
    if _,err=New(dir,initSource());err==nil {
        t.Fatal("malformed legacy effect journal was silently dropped")
    } else if !strings.Contains(err.Error(),"effects.jsonl"){
        t.Fatalf("missing diagnostic for corrupted journal: %v",err)
    }
}
func ExampleStore_Backup(){
    fmt.Println("SQLite snapshots use VACUUM INTO and never overwrite")
    // Output: SQLite snapshots use VACUUM INTO and never overwrite
}
