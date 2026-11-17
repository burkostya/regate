package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RestoreAcknowledgment is a non-secret local audit record. The operator's
// attestation is represented by its digest, not raw text or credentials.
type RestoreAcknowledgment struct {
	SnapshotSHA256 string    `json:"snapshot_sha256"`
	ReleasedSHA256 string    `json:"released_sha256"`
	Actor string             `json:"actor"`
	AttestationDigest string `json:"attestation_digest"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
	InvalidatedTasks int     `json:"invalidated_tasks"`
	DiscardedQueued int      `json:"discarded_queued"`
	RevokedGrants int        `json:"revoked_grants"`
	DiscardedCheckpoints int `json:"discarded_checkpoints"`
}

// AcknowledgeRestoredState requires a local operator to affirm that the
// original worker/upstream accesses have been fenced. The software CANNOT
// verify that assertion. It therefore invalidates ALL nonterminal tasks,
// queued inputs, suspended checkpoints and live OAuth grants before making
// the restored environment eligible for execution. No historical remote
// effects or accepted work can be implicitly replayed.
//
// Failures leave the marker in place. If a failure occurs after SQLite
// transaction commit, a fresh restore into a new empty directory is required;
// the operator must never manually delete the marker to bypass preflight.
func AcknowledgeRestoredState(stateDir, expectedSHA, actor, attestation string) (RestoreAcknowledgment, error) {
	var empty RestoreAcknowledgment
	if len(actor)<2 || len(actor)>128 || strings.TrimSpace(actor)!=actor ||
		strings.ContainsAny(actor,"\x00\r\n\t") ||
		len(attestation)<20 || len(attestation)>4096 ||
		strings.TrimSpace(attestation)!=attestation ||
		len(expectedSHA)!=64 {
		return empty, ErrUnsafeRestoreTarget
	}
	if _,err:=hex.DecodeString(expectedSHA);err!=nil{return empty,ErrUnsafeRestoreTarget}
	state,err:=os.Lstat(stateDir)
	if err!=nil{return empty,err}
	if !state.IsDir() || state.Mode().Perm()&0077!=0{return empty,ErrUnsafeRestoreTarget}
	markerPath:=filepath.Join(stateDir,restoreQuarantineFile)
	info,err:=os.Lstat(markerPath)
	if err!=nil{return empty,err}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077!=0 ||
		info.Size()>1024 {return empty,ErrUnsafeRestoreTarget}
	raw,err:=os.ReadFile(markerPath)
	if err!=nil{return empty,err}
	var marker restoreQuarantineRecord
	if json.Unmarshal(raw,&marker)!=nil || marker.SnapshotSHA256!=expectedSHA ||
		marker.RestoredAt.IsZero(){return empty,ErrUnsafeRestoreTarget}
	entries,err:=os.ReadDir(stateDir)
	if err!=nil{return empty,err}
	if len(entries)!=2{return empty,ErrUnsafeRestoreTarget}
	names:=map[string]bool{}
	for _,item:=range entries{names[item.Name()]=true}
	if !names[restoreQuarantineFile] || !names[databaseName]{
		return empty,ErrUnsafeRestoreTarget
	}
	database:=filepath.Join(stateDir,databaseName)
	original,err:=VerifyBackup(database)
	if err!=nil{return empty,err}
	if original.SHA256!=expectedSHA{return empty,ErrInvalidBackup}
	keyAudit,err:=VerifyBackupKeys(database)
	if err!=nil{return empty,err}
	if keyAudit.SnapshotSHA256!=expectedSHA{return empty,ErrInvalidBackup}

	absolute,err:=filepath.Abs(database)
	if err!=nil{return empty,err}
	fileURI:=(&url.URL{Scheme:"file",Path:absolute}).String()
	readOnly,err:=sql.Open("sqlite3",fileURI+"?mode=ro")
	if err!=nil{return empty,err}
	var leaseDeadline int64
	leaseErr:=readOnly.QueryRow("SELECT deadline_ns FROM execution_leases WHERE name=?",
		executionLeaseName).Scan(&leaseDeadline)
	closeErr:=readOnly.Close()
	if leaseErr!=nil && !errors.Is(leaseErr,sql.ErrNoRows){return empty,leaseErr}
	if closeErr!=nil{return empty,closeErr}
	if leaseErr==nil && leaseDeadline>time.Now().UnixNano(){
		return empty,ErrExecutionLeaseHeld
	}

	// A self-contained restored SQLite file has no WAL. Use a rollback
	// journal for the one-time offline mutation, so VerifyBackup can inspect
	// the resulting standalone file after the writer closes.
	writer,err:=sql.Open("sqlite3",fileURI+"?_txlock=immediate&_pragma=busy_timeout(15000)")
	if err!=nil{return empty,err}
	writer.SetMaxOpenConns(1)
	for _,pragma:=range []string{
		"PRAGMA journal_mode=DELETE","PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
	}{
		if _,err=writer.Exec(pragma);err!=nil{
			_=writer.Close()
			return empty,err
		}
	}
	receipt:=RestoreAcknowledgment{
		SnapshotSHA256:expectedSHA,Actor:actor,
		AcknowledgedAt:time.Now().UTC(),
	}
	digest:=sha256.Sum256([]byte(attestation))
	receipt.AttestationDigest=hex.EncodeToString(digest[:])
	err=invalidateHistoricalExecution(writer,&receipt)
	if closeErr:=writer.Close();err==nil{err=closeErr}
	if err!=nil{return empty,err}
	released,err:=VerifyBackup(database)
	if err!=nil{return empty,err}
	receipt.ReleasedSHA256=released.SHA256
	// The evidence receipt is durable BEFORE making execution possible.
	ackPath:=filepath.Join(stateDir,restoreReceiptFile)
	output,err:=os.OpenFile(ackPath,os.O_WRONLY|os.O_CREATE|os.O_EXCL,0600)
	if err!=nil{return empty,err}
	encoderErr:=json.NewEncoder(output).Encode(receipt)
	if encoderErr==nil{encoderErr=output.Sync()}
	if closeErr:=output.Close();encoderErr==nil{encoderErr=closeErr}
	if encoderErr!=nil{return empty,encoderErr}
	parent,err:=os.Open(stateDir)
	if err!=nil{return empty,err}
	err=parent.Sync()
	if err==nil{err=os.Remove(markerPath)}
	if err==nil{err=parent.Sync()}
	if closeErr:=parent.Close();err==nil{err=closeErr}
	if err!=nil{return empty,err}
	return receipt,nil
}

func invalidateHistoricalExecution(db *sql.DB, receipt *RestoreAcknowledgment) error {
	tx,err:=db.BeginTx(context.Background(),nil)
	if err!=nil{return err}
	defer tx.Rollback()
	rows,err:=tx.Query(`SELECT task_id,status,record FROM tasks
		WHERE status IN ('queued','working','input_required')`)
	if err!=nil{return err}
	type candidate struct{ id,status string; task TaskRecord }
	pending:=[]candidate{}
	for rows.Next(){
		var id,status,raw string
		if err=rows.Scan(&id,&status,&raw);err!=nil{break}
		var task TaskRecord
		if err=json.Unmarshal([]byte(raw),&task);err!=nil{break}
		if task.TaskID!=id || task.Status!=status || len(id)!=24 {
			err=ErrInvalidBackup
			break
		}
		pending=append(pending,candidate{id:id,status:status,task:task})
	}
	if err==nil{err=rows.Err()}
	if closeErr:=rows.Close();err==nil{err=closeErr}
	if err!=nil{return ErrInvalidBackup}
	for _,item:=range pending{
		task:=item.task
		task.Status="failed"
		task.Phase=""
		task.InputRequests=nil
		task.StatusMessage="Historical task invalidated during restored-state acknowledgment"
		task.Error=map[string]any{
			"code":-32000,
			"message":"Historical task is not replayable after restore; submit a new authorized request",
		}
		task.LastUpdatedAt=time.Now().UTC()
		raw,err:=json.Marshal(task)
		if err!=nil{return err}
		res,err:=tx.Exec(`UPDATE tasks SET status='failed',record=?
			WHERE task_id=? AND status=?`,string(raw),item.id,item.status)
		if err!=nil{return err}
		affected,err:=res.RowsAffected()
		if err!=nil || affected!=1{return ErrInvalidBackup}
		receipt.InvalidatedTasks++
	}
	if err=tx.QueryRow("SELECT COUNT(*) FROM task_queue").Scan(&receipt.DiscardedQueued);
		err!=nil{return err}
	if _,err=tx.Exec("DELETE FROM task_queue");err!=nil{return err}
	if _,err=tx.Exec("DELETE FROM task_queue_turns");err!=nil{return err}
	res,err:=tx.Exec(`UPDATE task_grants SET state='revoked',
		generation=generation+1,nonce=X'',ciphertext=X'',updated_ns=?
		WHERE state IN ('active','refreshing')`,time.Now().UnixNano())
	if err!=nil{return err}
	affected,err:=res.RowsAffected()
	if err!=nil{return err}
	receipt.RevokedGrants=int(affected)
	if err=tx.QueryRow("SELECT COUNT(*) FROM suspended").Scan(&receipt.DiscardedCheckpoints);
		err!=nil{return err}
	if _,err=tx.Exec("DELETE FROM suspended");err!=nil{return err}
	return tx.Commit()
}
