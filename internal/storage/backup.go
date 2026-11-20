package storage

import (
    "context"
    "crypto/sha256"
    "database/sql"
    "encoding/hex"
    "errors"
    "fmt"
    "io"
    "net/url"
    "os"
    "path/filepath"
    "strings"
)

var ErrInvalidBackup = errors.New("Regate SQLite backup failed verification")

// BackupManifest contains structural, application-level and byte-integrity
// evidence about a standalone SQLite snapshot. It intentionally contains no
// HCL source, queued arguments, OAuth credentials or token material.
type BackupManifest struct {
    SchemaVersion int `json:"schema_version"`
    ActiveRevision string `json:"active_revision"`
    Revisions int `json:"revisions"`
    Effects int `json:"effects"`
    AuditEvents int `json:"audit_events"`
    Tasks int `json:"tasks"`
    Queued int `json:"queued"`
    Grants int `json:"grants"`
    SizeBytes int64 `json:"size_bytes"`
    SHA256 string `json:"sha256"`
}

const latestSQLiteVersion = 7

// VerifyBackup inspects a standalone SQLite snapshot without ever calling
// Store.New (which could run recovery/migrations). It refuses symlinks,
// publicly readable files, corrupt SQLite pages, broken revision chains,
// orphan current-effect projections and invalid current-schema versions.
//
// An integrity check cannot prove that a remote effect or grant is semantically
// correct, or that the external encryption keyring is still available.
func VerifyBackup(path string)(BackupManifest,error) {
    var manifest BackupManifest
    info,err:=os.Lstat(path)
    if err!=nil{return manifest,err}
    if !info.Mode().IsRegular() || info.Mode().Perm()&0077!=0 {
        return manifest,ErrInvalidBackup
    }
    absolute,err:=filepath.Abs(path)
    if err!=nil{return manifest,err}
    uri:=(&url.URL{Scheme:"file",Path:absolute}).String()+"?mode=ro"
    db,err:=sql.Open("sqlite3",uri)
    if err!=nil{return manifest,ErrInvalidBackup}
    defer db.Close()
    if _,err=db.Exec("PRAGMA query_only=ON");err!=nil {
        return manifest,ErrInvalidBackup
    }
    var check string
    if err=db.QueryRow("PRAGMA integrity_check").Scan(&check);err!=nil || check!="ok" {
        return manifest,ErrInvalidBackup
    }
    var version int
    if err=db.QueryRow("PRAGMA user_version").Scan(&version);err!=nil ||
        version<6 || version>latestSQLiteVersion {
        return manifest,ErrInvalidBackup
    }
    manifest.SchemaVersion=version
    if version >= 7 {
        var principals int
        if err=db.QueryRow("SELECT count(*) FROM oauth_quota").Scan(&principals);err!=nil || principals>maxOAuthQuotaPrincipals {
            return BackupManifest{},ErrInvalidBackup
        }
    }

    // Even a structurally intact SQLite snapshot may have a forged or
    // truncated management audit chain. Reject it *before* publishing or
    // allowing recovery to proceed.
    auditRows,auditErr:=db.Query(auditSelect+" ORDER BY seq")
    if auditErr!=nil{return BackupManifest{},ErrInvalidBackup}
    auditCount,_,auditErr:=verifyAuditRows(auditRows)
    if auditErr!=nil {return BackupManifest{},ErrInvalidBackup}
    manifest.AuditEvents=int(auditCount)

    rows,err:=db.Query(`SELECT id,digest,parent,source
        FROM revisions ORDER BY seq`)
    if err!=nil{return manifest,ErrInvalidBackup}
    prior:=""
    for rows.Next(){
        var id,digest,parent,source string
        if err=rows.Scan(&id,&digest,&parent,&source);err!=nil {break}
        if len(id)!=20 || len(digest)!=20 || digest!=Hash(source) ||
            parent!=prior {
            err=ErrInvalidBackup
            break
        }
        prior=id
        manifest.Revisions++
    }
    if err==nil{err=rows.Err()}
    if closeErr:=rows.Close();err==nil{err=closeErr}
    if err!=nil || manifest.Revisions==0{return BackupManifest{},ErrInvalidBackup}
    manifest.ActiveRevision=prior

    // Every current effect projection must reference the newest event for
    // that effect, with the same status. This is a durability invariant,
    // not a claim that the remote action definitely succeeded.
    var inconsistent int
    if err=db.QueryRow(`SELECT COUNT(*) FROM (
        SELECT effect_id,MAX(seq) latest FROM effect_events GROUP BY effect_id
    ) latest
    LEFT JOIN effect_current current ON current.effect_id=latest.effect_id
    LEFT JOIN effect_events event ON event.seq=latest.latest
    WHERE current.seq IS NULL OR current.seq<>latest.latest
       OR current.status<>event.status`).Scan(&inconsistent);
        err!=nil || inconsistent!=0 {
        return BackupManifest{},ErrInvalidBackup
    }
    // Reject current records with no corresponding source event.
    if err=db.QueryRow(`SELECT COUNT(*) FROM effect_current current
        LEFT JOIN effect_events e ON e.seq=current.seq
            AND e.effect_id=current.effect_id
        WHERE e.seq IS NULL OR e.status<>current.status`).
        Scan(&inconsistent);err!=nil || inconsistent!=0 {
        return BackupManifest{},ErrInvalidBackup
    }
    for _,query:=range []struct{sql string;out *int}{
        {"SELECT COUNT(*) FROM effect_current",&manifest.Effects},
        {"SELECT COUNT(*) FROM tasks",&manifest.Tasks},
        {"SELECT COUNT(*) FROM task_queue",&manifest.Queued},
        {"SELECT COUNT(*) FROM task_grants",&manifest.Grants},
    }{
        if err=db.QueryRow(query.sql).Scan(query.out);err!=nil{
            return BackupManifest{},ErrInvalidBackup
        }
    }
    // PRAGMA foreign_key_check emits rows, not the "ok" string of integrity
    // checking. A violated queued task/grant FK is a broken snapshot.
    fkRows,err:=db.Query("PRAGMA foreign_key_check")
    if err!=nil{return BackupManifest{},ErrInvalidBackup}
    if fkRows.Next(){_ = fkRows.Close();return BackupManifest{},ErrInvalidBackup}
    if err=fkRows.Err();err!=nil{_ = fkRows.Close();return BackupManifest{},ErrInvalidBackup}
    if err=fkRows.Close();err!=nil{return BackupManifest{},ErrInvalidBackup}

    file,err:=os.Open(absolute)
    if err!=nil{return BackupManifest{},err}
    digest:=sha256.New()
    copied,err:=io.Copy(digest,file)
    if closeErr:=file.Close();err==nil{err=closeErr}
    if err!=nil{return BackupManifest{},err}
    if copied!=info.Size(){return BackupManifest{},ErrInvalidBackup}
    manifest.SizeBytes=copied
    manifest.SHA256=hex.EncodeToString(digest.Sum(nil))
    return manifest,nil
}

// BackupVerified writes a VACUUM snapshot into a *private* staging directory
// within the destination filesystem, validates the snapshot, syncs it and
// publishes it via a no-overwrite hard link. A failed or interrupted attempt
// cannot expose a partial destination under its final name.
func (s *Store) BackupVerified(destination string)(BackupManifest,error) {
    var empty BackupManifest
    if strings.TrimSpace(destination)==""{return empty,fmt.Errorf("empty backup path")}
    absolute,err:=filepath.Abs(destination)
    if err!=nil{return empty,err}
    if _,err=os.Lstat(absolute);err==nil {
        return empty,fmt.Errorf("backup already exists")
    }else if !errors.Is(err,os.ErrNotExist){return empty,err}

    stagingDir,err:=os.MkdirTemp(filepath.Dir(absolute),".regate-backup-*")
    if err!=nil{return empty,err}
    defer os.RemoveAll(stagingDir)
    if err=os.Chmod(stagingDir,0700);err!=nil{return empty,err}
    tempPath:=filepath.Join(stagingDir,"snapshot.sqlite")
    if _,err=s.db.ExecContext(context.Background(),"VACUUM INTO ?",tempPath);err!=nil {
        return empty,err
    }
    if err=os.Chmod(tempPath,0600);err!=nil{return empty,err}
    handle,err:=os.Open(tempPath)
    if err!=nil{return empty,err}
    err=handle.Sync()
    if closeErr:=handle.Close();err==nil{err=closeErr}
    if err!=nil{return empty,err}

    manifest,err:=VerifyBackup(tempPath)
    if err!=nil{return empty,err}
    // Unlike os.Rename, os.Link does not overwrite an existing path, even
    // when another backup process won a race after the initial Lstat.
    // The private staging directory and final backup share one filesystem.
    if err=os.Link(tempPath,absolute);err!=nil{return empty,err}
    // Sync the containing directory for the new hard-link directory entry.
    // If fsync fails, surface it without falsely claiming durable success.
    parent,err:=os.Open(filepath.Dir(absolute))
    if err!=nil{return empty,err}
    err=parent.Sync()
    if closeErr:=parent.Close();err==nil{err=closeErr}
    if err!=nil{return empty,err}
    return manifest,nil
}
