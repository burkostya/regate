package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRestoredStateAcknowledgmentInvalidatesHistoricalWork(t *testing.T) {
	testGrantEnv(t,"grant",map[string]string{"grant":testGrantKey(t)})
	setQueueKeyRing(t)
	source,err:=New(t.TempDir(),initSource())
	if err!=nil{t.Fatal(err)}
	defer source.Close()
	ctx:=context.Background()
	busy:=capacityTask(source,"working")
	if queued,err:=source.AdmitOrQueueTask(ctx,busy,map[string]any{"job":"busy"},1,2);
		err!=nil || queued{t.Fatalf("busy fixture: %v",err)}
	queued:=capacityTask(source,"working")
	if deferred,err:=source.AdmitOrQueueTask(ctx,queued,
		map[string]any{"secret":"historical-queued-input"},1,2);
		err!=nil || !deferred{t.Fatalf("encrypted queue fixture: %v",err)}
	delegated:=testGrantTask(t,source)
	if _,err:=source.CreateTaskGrant(ctx,delegated.TaskID,delegated.OwnerDigest,
		"historical-refresh-token",expiresGrant());err!=nil{t.Fatal(err)}
	checkpoint:=ID()
	if err:=source.PutSuspended(checkpoint,map[string]any{"past":"unsafe-replay"});
		err!=nil{t.Fatal(err)}
	effect:=Effect{ID:ID(),RunID:ID(),Node:"send",
		Operation:"resource.write",Status:"unknown"}
	if err:=source.Effect(effect);err!=nil{t.Fatal(err)}
	archive:=filepath.Join(t.TempDir(),"snapshot.sqlite")
	original,err:=source.BackupVerified(archive)
	if err!=nil{t.Fatal(err)}
	dir:=privateEmptyDir(t)
	if _,err:=RestoreVerifiedBackup(archive,dir);err!=nil{t.Fatal(err)}
	if observer,err:=New(dir,initSource());!errors.Is(err,ErrRestoreQuarantined){
		if observer!=nil{_ = observer.Close()}
		t.Fatalf("quarantined snapshot admitted as live: %v",err)
	}
	attestation:="Original worker is fenced and new requests must be explicitly authorized."
	receipt,err:=AcknowledgeRestoredState(dir,original.SHA256,
		"recovery-operator",attestation)
	if err!=nil{t.Fatal(err)}
	if receipt.SnapshotSHA256!=original.SHA256 ||
		receipt.ReleasedSHA256==original.SHA256 ||
		receipt.InvalidatedTasks!=3 || receipt.DiscardedQueued!=1 ||
		receipt.RevokedGrants!=1 || receipt.DiscardedCheckpoints!=1 ||
		len(receipt.AttestationDigest)!=64{
		t.Fatalf("unsafe acknowledgment report: %#v",receipt)
	}
	if _,err:=os.Lstat(filepath.Join(dir,restoreQuarantineFile));
		!errors.Is(err,os.ErrNotExist){
		t.Fatalf("quarantine marker remained: %v",err)
	}
	raw,err:=os.ReadFile(filepath.Join(dir,restoreReceiptFile))
	if err!=nil{t.Fatal(err)}
	for _,secret:=range []string{
		"historical-queued-input","historical-refresh-token",attestation,
		delegated.OwnerDigest,
	}{
		if bytes.Contains(raw,[]byte(secret)){
			t.Fatal("sensitive historical state or raw attestation leaked into audit record")
		}
	}
	var persisted RestoreAcknowledgment
	if err:=json.Unmarshal(raw,&persisted);err!=nil || persisted!=receipt{
		t.Fatalf("acknowledgment not durable: %#v %v",persisted,err)
	}
	reopened,err:=New(dir,initSource())
	if err!=nil{t.Fatal(err)}
	defer reopened.Close()
	if err:=reopened.AcquireExecutionLease();err!=nil{t.Fatal(err)}
	for _,task:=range []TaskRecord{busy,queued,delegated}{
		record,err:=reopened.TaskGet(task.TaskID)
		if err!=nil || record.Status!="failed"{
			t.Fatalf("historic task could replay: %#v %v",record,err)
		}
	}
	if n,err:=reopened.QueueDepth(ctx);err!=nil || n!=0{
		t.Fatalf("queued input still executable: %d %v",n,err)
	}
	var grantState string
	var bytesRemaining int
	if err:=reopened.db.QueryRow("SELECT state,length(ciphertext) FROM task_grants WHERE task_id=?",
		delegated.TaskID).Scan(&grantState,&bytesRemaining);
		err!=nil || grantState!="revoked" || bytesRemaining!=0{
		t.Fatalf("historical grant not revoked: %s %d %v",grantState,bytesRemaining,err)
	}
	var cp any
	if err:=reopened.GetSuspended(checkpoint,&cp);!errors.Is(err,sql.ErrNoRows){
		t.Fatalf("historical checkpoint still resumable: %v",err)
	}
	unchanged,err:=reopened.EffectGet(effect.ID)
	if err!=nil || unchanged.Status!="unknown"{
		t.Fatalf("unknown external effect evidence lost: %#v %v",unchanged,err)
	}
}

func TestRestoredStateCannotBeReleasedWithoutKeys(t *testing.T) {
	testGrantEnv(t,"grant",map[string]string{"grant":testGrantKey(t)})
	setQueueKeyRing(t)
	s,err:=New(t.TempDir(),initSource())
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	ctx:=context.Background()
	active:=capacityTask(s,"working")
	if _,err:=s.AdmitOrQueueTask(ctx,active,map[string]any{},1,2);err!=nil{t.Fatal(err)}
	waiting:=capacityTask(s,"working")
	if _,err:=s.AdmitOrQueueTask(ctx,waiting,map[string]any{"private":"payload"},1,2);
		err!=nil{t.Fatal(err)}
	archive:=filepath.Join(t.TempDir(),"backup.sqlite")
	original,err:=s.BackupVerified(archive)
	if err!=nil{t.Fatal(err)}
	dir:=privateEmptyDir(t)
	if _,err:=RestoreVerifiedBackup(archive,dir);err!=nil{t.Fatal(err)}
	t.Setenv(taskQueueKeyEnv,"")
	if _,err:=AcknowledgeRestoredState(dir,original.SHA256,
		"operator","Source resources are fenced and historical queue will be invalidated.");
		!errors.Is(err,ErrTaskQueueKeyUnavailable){
		t.Fatalf("missing queue key was accepted: %v",err)
	}
	if _,err:=os.Stat(filepath.Join(dir,restoreQuarantineFile));err!=nil{
		t.Fatalf("failed key audit lifted quarantine: %v",err)
	}
	if _,err:=AcknowledgeRestoredState(dir,
		"0000000000000000000000000000000000000000000000000000000000000000",
		"operator","Source resources are fenced and historical queue will be invalidated.");
		!errors.Is(err,ErrUnsafeRestoreTarget){
		t.Fatalf("incorrect snapshot digest accepted: %v",err)
	}
}

func TestRestoredStateCannotBypassCopiedLiveLease(t *testing.T){
	t.Setenv(taskQueueKeyEnv,"")
	t.Setenv(grantKeyringEnv,"")
	s,err:=New(t.TempDir(),initSource())
	if err!=nil{t.Fatal(err)}
	defer s.Close()
	if err:=s.AcquireExecutionLease();err!=nil{t.Fatal(err)}
	archive:=filepath.Join(t.TempDir(),"snapshot.sqlite")
	m,err:=s.BackupVerified(archive)
	if err!=nil{t.Fatal(err)}
	dir:=privateEmptyDir(t)
	if _,err:=RestoreVerifiedBackup(archive,dir);err!=nil{t.Fatal(err)}
	if _,err:=AcknowledgeRestoredState(dir,m.SHA256,"operator",
		"Remote worker has been fenced but copied lease deadline has not yet passed.");
		!errors.Is(err,ErrExecutionLeaseHeld){
		t.Fatalf("live copied lease was ignored: %v",err)
	}
	if _,err:=os.Stat(filepath.Join(dir,restoreQuarantineFile));err!=nil{
		t.Fatalf("live lease failure removed quarantine: %v",err)
	}
}
