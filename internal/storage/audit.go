package storage

import (
    "context"
    "crypto/sha256"
    "database/sql"
    "encoding/hex"
    "errors"
    "fmt"
    "time"
)

// AuditRecord intentionally has no payload, credentials, remote address,
// principal name, arbitrary message or error text. The digest chain detects
// record corruption, invalid digests and gaps in the stored sequence.
type AuditRecord struct {
    Sequence int64 `json:"sequence"`
    At string `json:"at"`
    Category string `json:"category"`
    Action string `json:"action"`
    Outcome string `json:"outcome"`
    PrevDigest string `json:"prev_digest"`
    Digest string `json:"digest"`
}

const auditGenesis = "0000000000000000000000000000000000000000000000000000000000000000"

func safeAuditAction(action string) bool {
    if len(action)<1 || len(action)>80 {return false}
    for i,c:=range action {
        if c>='a' && c<='z' {continue}
        if i>0 && ((c>='0' && c<='9') || c=='_') {continue}
        return false
    }
    return true
}

func auditDigest(e AuditRecord) string {
    // Domain-separated and field-delimited canonical data; all fields are
    // validated and record content is independent of user-supplied arguments.
    sum:=sha256.Sum256([]byte(fmt.Sprintf("regate/audit/v1\x00%d\x00%s\x00%s\x00%s\x00%s\x00%s",
        e.Sequence,e.At,e.Category,e.Action,e.Outcome,e.PrevDigest)))
    return hex.EncodeToString(sum[:])
}

func verifyAuditRows(rows *sql.Rows)(int64,string,error) {
    defer rows.Close()
    lastSeq:=int64(0)
    previous:=auditGenesis
    for rows.Next() {
        var e AuditRecord
        if err:=rows.Scan(&e.Sequence,&e.At,&e.Category,&e.Action,&e.Outcome,&e.PrevDigest,&e.Digest);err!=nil {return 0,"",err}
        if e.Sequence!=lastSeq+1 || e.PrevDigest!=previous || !safeAuditAction(e.Action) ||
            (e.Category!="management" && e.Category!="authentication") ||
            (e.Outcome!="started" && e.Outcome!="succeeded" && e.Outcome!="failed" && e.Outcome!="denied") {
            return 0,"",fmt.Errorf("audit chain sequence %d has invalid provenance",e.Sequence)
        }
        if _,err:=time.Parse(time.RFC3339Nano,e.At);err!=nil {
            return 0,"",fmt.Errorf("audit chain sequence %d has invalid timestamp",e.Sequence)
        }
        if e.Digest!=auditDigest(e) {
            return 0,"",fmt.Errorf("audit chain sequence %d failed digest verification",e.Sequence)
        }
        lastSeq=e.Sequence
        previous=e.Digest
    }
    if err:=rows.Err();err!=nil{return 0,"",err}
    return lastSeq,previous,nil
}

const auditSelect = "SELECT seq,at,category,action,outcome,prev_digest,digest FROM audit_events"

func verifyAuditTx(tx *sql.Tx) error {
    rows,err:=tx.Query(auditSelect+" ORDER BY seq")
    if err!=nil{return err}
    _,_,err=verifyAuditRows(rows)
    return err
}

// AuditVerify checks the entire local chain. Callers must not mistake a
// self-hashed SQLite record for a third-party cryptographic attestation.
func (s *Store) AuditVerify() error {
    rows,err:=s.db.Query(auditSelect+" ORDER BY seq")
    if err!=nil{return err}
    _,_,err=verifyAuditRows(rows)
    return err
}

// AppendAudit verifies the chain and atomically appends the event. It is used
// as a BEFORE barrier for administrative mutations; if journaling is broken
// no protected state transition may start.
func (s *Store) AppendAudit(category,action,outcome string) error {
    if (category!="management" && category!="authentication") || !safeAuditAction(action) {
        return errors.New("invalid audit event category/action")
    }
    switch outcome {
    case "started","succeeded","failed","denied":
    default: return errors.New("invalid audit event outcome")
    }
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return err}
    defer tx.Rollback()
    rows,err:=tx.Query(auditSelect+" ORDER BY seq")
    if err!=nil{return err}
    last,previous,err:=verifyAuditRows(rows)
    if err!=nil{return err}
    e:=AuditRecord{
        Sequence:last+1,At:time.Now().UTC().Format(time.RFC3339Nano),
        Category:category,Action:action,Outcome:outcome,PrevDigest:previous,
    }
    e.Digest=auditDigest(e)
    _,err=tx.Exec(`INSERT INTO audit_events(seq,at,category,action,outcome,prev_digest,digest)
        VALUES(?,?,?,?,?,?,?)`,
        e.Sequence,e.At,e.Category,e.Action,e.Outcome,e.PrevDigest,e.Digest)
    if err!=nil{return err}
    return tx.Commit()
}

// AuditTail is a bounded, newest-first read for authenticated operators.
// Full verification precedes every read so tampering cannot be hidden by a
// recent valid suffix. The endpoint never exposes arbitrary stored messages.
func (s *Store) AuditTail(limit int)([]AuditRecord,error) {
    if limit<=0 {limit=100}
    if limit>200 {limit=200}
    if err:=s.AuditVerify();err!=nil{return nil,err}
    rows,err:=s.db.Query(auditSelect+" ORDER BY seq DESC LIMIT ?",limit)
    if err!=nil{return nil,err}
    defer rows.Close()
    records:=make([]AuditRecord,0,limit)
    for rows.Next() {
        var e AuditRecord
        if err:=rows.Scan(&e.Sequence,&e.At,&e.Category,&e.Action,&e.Outcome,&e.PrevDigest,&e.Digest);err!=nil {return nil,err}
        records=append(records,e)
    }
    if err:=rows.Err();err!=nil{return nil,err}
    return records,nil
}
