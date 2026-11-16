package storage

import (
    "context"
    "database/sql"
    "errors"
    "os"
    "path/filepath"
    "sync"
    "testing"
)

func TestVerifiedBackupContainsDurableDomainsAndStableManifest(t *testing.T) {
    testGrantEnv(t,"k1",map[string]string{"k1":testGrantKey(t)})
    s:=queueStore(t)
    busy:=capacityTask(s,"working")
    if _,err:=s.AdmitOrQueueTask(context.Background(),busy,map[string]any{"id":"busy"},1,3);
        err!=nil{t.Fatal(err)}
    queued:=capacityTask(s,"working")
    if admitted,err:=s.AdmitOrQueueTask(context.Background(),queued,
        map[string]any{"private":"hidden-queue-input"},1,3);
        err!=nil || !admitted {t.Fatalf("queued task admission failed: %v",err)}
    delegated:=testGrantTask(t,s)
    if _,err:=s.CreateTaskGrant(context.Background(),delegated.TaskID,
        delegated.OwnerDigest,"refresh-secret",expiresGrant());err!=nil{
        t.Fatal(err)
    }
    effect:=Effect{ID:ID(),RunID:ID(),Node:"write",
        Operation:"resource.update",Status:"confirmed"}
    if err:=s.Effect(effect);err!=nil{t.Fatal(err)}

    destination:=filepath.Join(t.TempDir(),"snapshot.sqlite")
    manifest,err:=s.BackupVerified(destination)
    if err!=nil{t.Fatal(err)}
    if manifest.SchemaVersion!=latestSQLiteVersion ||
        manifest.Revisions!=1 || manifest.Tasks!=3 ||
        manifest.Queued!=1 || manifest.Grants!=1 || manifest.Effects!=1 ||
        len(manifest.SHA256)!=64 || manifest.SizeBytes==0 {
        t.Fatalf("verified manifest inaccurate: %#v",manifest)
    }
    info,err:=os.Stat(destination)
    if err!=nil{t.Fatal(err)}
    if info.Mode().Perm()!=0600 {t.Fatalf("snapshot permissions %#o",info.Mode().Perm())}
    repeated,err:=VerifyBackup(destination)
    if err!=nil || repeated!=manifest {
        t.Fatalf("stable verified snapshot changed: %#v %#v %v",
            manifest,repeated,err)
    }
    if err=s.Backup(destination);err==nil {
        t.Fatal("existing verified backup silently overwritten")
    }
    after,err:=VerifyBackup(destination)
    if err!=nil || after!=manifest {
        t.Fatalf("overwrite attempt changed backup: %#v %v",after,err)
    }
    entries,err:=os.ReadDir(filepath.Dir(destination))
    if err!=nil{t.Fatal(err)}
    if len(entries)!=1 || entries[0].Name()!="snapshot.sqlite"{
        t.Fatalf("backup leaked private staging files: %#v",entries)
    }
}

func TestVerifyBackupRejectsCorruptedRevisionAndEffectProjection(t *testing.T) {
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    effect:=Effect{ID:ID(),RunID:ID(),Node:"write",
        Operation:"crm.create",Status:"confirmed"}
    if err=s.Effect(effect);err!=nil{t.Fatal(err)}

    for _,mutation:=range []struct{name,sql string}{
        {"revision-digest",`UPDATE revisions SET source='tampered'`},
        {"effect-projection",`UPDATE effect_current SET seq=9999`},
    }{
        t.Run(mutation.name,func(t *testing.T){
            backup:=filepath.Join(t.TempDir(),"mutated.sqlite")
            if err:=s.Backup(backup);err!=nil{t.Fatal(err)}
            db,err:=sql.Open("sqlite3","file:"+backup)
            if err!=nil{t.Fatal(err)}
            if _,err=db.Exec(mutation.sql);err!=nil{t.Fatal(err)}
            if err=db.Close();err!=nil{t.Fatal(err)}
            if _,err=VerifyBackup(backup);!errors.Is(err,ErrInvalidBackup){
                t.Fatalf("application-level corrupted snapshot was trusted: %v",err)
            }
        })
    }
}

func TestVerifyBackupRejectsTruncationPermissionsAndSymlinks(t *testing.T){
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    backup:=filepath.Join(t.TempDir(),"snapshot.sqlite")
    if err=s.Backup(backup);err!=nil{t.Fatal(err)}
    if err=os.Chmod(backup,0644);err!=nil{t.Fatal(err)}
    if _,err=VerifyBackup(backup);!errors.Is(err,ErrInvalidBackup){
        t.Fatalf("publicly readable snapshot passed verification: %v",err)
    }
    if err=os.Chmod(backup,0600);err!=nil{t.Fatal(err)}
    alias:=filepath.Join(filepath.Dir(backup),"snapshot-alias.sqlite")
    if err=os.Symlink(backup,alias);err!=nil{t.Fatal(err)}
    if _,err=VerifyBackup(alias);!errors.Is(err,ErrInvalidBackup){
        t.Fatalf("symlink snapshot accepted: %v",err)
    }
    if err=s.Backup(alias);err==nil {t.Fatal("backup followed existing symlink")}
    if err=os.Truncate(backup,128);err!=nil{t.Fatal(err)}
    if _,err=VerifyBackup(backup);!errors.Is(err,ErrInvalidBackup){
        t.Fatalf("truncated snapshot passed verification: %v",err)
    }
}

func TestParallelBackupPublicationNeverOverwritesDestination(t *testing.T){
    s,err:=New(t.TempDir(),initSource())
    if err!=nil{t.Fatal(err)}
    defer s.Close()
    dest:=filepath.Join(t.TempDir(),"snapshot.sqlite")
    const clients=2
    var wg sync.WaitGroup
    ok:=make(chan BackupManifest,clients)
    rejected:=make(chan error,clients)
    for i:=0;i<clients;i++{
        wg.Add(1)
        go func(){
            defer wg.Done()
            manifest,err:=s.BackupVerified(dest)
            if err!=nil {rejected<-err}else{ok<-manifest}
        }()
    }
    wg.Wait()
    close(ok);close(rejected)
    if len(ok)!=1 || len(rejected)!=1 {
        t.Fatalf("parallel publication: successes %d failures %d",len(ok),len(rejected))
    }
    verified,err:=VerifyBackup(dest)
    if err!=nil{t.Fatal(err)}
    winner:=<-ok
    if verified.SHA256!=winner.SHA256 {
        t.Fatalf("destination corrupted by losing writer: %#v %#v",verified,winner)
    }
    entries,err:=os.ReadDir(filepath.Dir(dest))
    if err!=nil{t.Fatal(err)}
    if len(entries)!=1 {t.Fatalf("failed writer leaked temporary backup: %#v",entries)}
}
