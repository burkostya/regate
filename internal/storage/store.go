// Package storage persists Regate's revisions, drafts, tasks, effects,
// checkpoints and observability records in a single transactional SQLite DB.
//
// Files from pre-SQLite releases are imported in one transaction on first
// startup and retained unmodified as a rollback/forensic backup.
package storage

import (
    "context"
    "crypto/rand"
    "crypto/sha256"
    "database/sql"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "net/url"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "sync"
    "time"

    _ "github.com/ncruces/go-sqlite3/driver"
    "github.com/burkostya/regate/internal/expr"
)

func ID() string {
    b := make([]byte,12)
    if _,err:=rand.Read(b);err!=nil {panic("secure random generator failed: "+err.Error())}
    return hex.EncodeToString(b)
}
func Hash(s string) string {h:=sha256.Sum256([]byte(s));return hex.EncodeToString(h[:])[:20]}
// SourceDigest is the full SHA-256 of exactly the stored editable HCL bytes,
// not the shorter public revision identifier.
func SourceDigest(source string) string {
    sum := sha256.Sum256([]byte(source))
    return hex.EncodeToString(sum[:])
}
var ErrDraftSourceConflict = errors.New("draft source conflict: stale editor snapshot")

type Draft struct {
    ID string `json:"id"`
    Base string `json:"base"`
    Source string `json:"source"`
    SourceDigest string `json:"source_digest"`
    Created time.Time `json:"created"`
}
type Revision struct {
    ID string `json:"id"`
    Digest string `json:"digest"`
    Parent string `json:"parent,omitempty"`
    Activated time.Time `json:"activated"`
}
type Trace struct {
    RunID string `json:"run_id"`
    Revision string `json:"revision"`
    Surface string `json:"surface"`
    Tool string `json:"tool"`
    Node string `json:"node,omitempty"`
    Status string `json:"status"`
    Message string `json:"message,omitempty"`
    At time.Time `json:"at"`
    DurationMS int64 `json:"duration_ms,omitempty"`
}
type Effect struct {
    ID string `json:"id"`
    RunID string `json:"run_id"`
    Node string `json:"node"`
    Operation string `json:"operation"`
    IntentDigest string `json:"intent_digest,omitempty"`
    TargetDigest string `json:"target_digest,omitempty"`
    Status string `json:"status"`
    At time.Time `json:"at"`
    Reason string `json:"reason,omitempty"`
    Resolution *EffectResolution `json:"resolution,omitempty"`
    // Compensation is a separate effect with its own durable dispatch barrier.
    Compensates string `json:"compensates,omitempty"`
    Approval *CompensationApproval `json:"compensation_approval,omitempty"`
}

// The single-connection pool makes all store-level operations serial without
// requiring CGO. Cross-process writers are coordinated by SQLite's WAL and
// BEGIN IMMEDIATE (configured in the driver DSN), not by an in-memory mutex.
type Store struct {
    Dir string
    db *sql.DB
    mu sync.RWMutex
    leaseOwner string
    leaseEpoch int64
    leaseLost bool
    leaseStop chan struct{}
    leaseDone chan struct{}
    grantKeys *GrantKeyRing
    queueKeys *TaskQueueKeyRing
    observationKey [32]byte
    suspendedRunMu sync.Mutex
    suspendedRuns map[string]*suspendedRunLock
}

// A run guard is shared by all Runtime objects using one execution-leader
// Store. Reference counts include waiters, so eviction never replaces the
// mutex while a concurrent resume is waiting for it.
type suspendedRunLock struct {
    mu sync.Mutex
    references int
}

// LockSuspendedRun serializes read/validate/answer/execute/delete for one
// durable run. The caller must hold the guard through effectful execution.
// Different run IDs are independent. Cross-process execution is fenced by
// the SQLite execution leader lease, not this local mutex.
func (s *Store) LockSuspendedRun(runID string) func() {
    s.suspendedRunMu.Lock()
    if s.suspendedRuns == nil { s.suspendedRuns = map[string]*suspendedRunLock{} }
    lock := s.suspendedRuns[runID]
    if lock == nil {
        lock = &suspendedRunLock{}
        s.suspendedRuns[runID] = lock
    }
    lock.references++
    s.suspendedRunMu.Unlock()

    lock.mu.Lock()
    return func() {
        lock.mu.Unlock()
        s.suspendedRunMu.Lock()
        lock.references--
        if lock.references == 0 { delete(s.suspendedRuns, runID) }
        s.suspendedRunMu.Unlock()
    }
}

const databaseName = "regate.sqlite"

func New(dir,initial string) (*Store,error) {
    return Open(dir, func() (string, string, error) { return initial, "", nil })
}

// Open calls bootstrap only when the database has no current or legacy revision.
// The callback returns the effective HCL and its static prefix, if any.
func Open(dir string, bootstrap func() (source, staticPrefix string, err error)) (*Store,error) {
    absolute, err := filepath.Abs(dir)
    if err != nil {
        return nil, err
    }
    dir = absolute
    // Do not even open a quarantined restore: initialization can otherwise
    // migrate state or reconcile interrupted work before operator review.
    if err:=checkRestoreQuarantine(dir);err!=nil{return nil,err}
    if err:=os.MkdirAll(dir,0700);err!=nil{return nil,err}
    path:=filepath.Join(dir,databaseName)
    uri:=(&url.URL{Scheme:"file",Path:path}).String()+
        "?_txlock=immediate&_pragma=busy_timeout(15000)"
    db,err:=sql.Open("sqlite3",uri)
    if err!=nil{return nil,err}
    db.SetMaxOpenConns(1)
    db.SetMaxIdleConns(1)
    keys,err:=LoadGrantKeyRing()
    if err!=nil {_=db.Close();return nil,fmt.Errorf("grant key configuration: %w",err)}
    queueKeys,err:=LoadTaskQueueKeyRing()
    if err!=nil {_=db.Close();return nil,fmt.Errorf("task queue key configuration: %w",err)}
    s:=&Store{Dir:dir,db:db,grantKeys:keys,queueKeys:queueKeys}
    if err=s.initialize(bootstrap);err!=nil {
        _=db.Close()
        return nil,err
    }
    return s,nil
}
func (s *Store) Close() error {
    s.releaseExecutionLease()
    return s.db.Close()
}

func (s *Store) initialize(bootstrap func() (string, string, error)) error {
    for _,statement:=range []string{
        "PRAGMA journal_mode=WAL",
        "PRAGMA synchronous=FULL",
        "PRAGMA foreign_keys=ON",
    } {
        if _,err:=s.db.Exec(statement);err!=nil{return fmt.Errorf("SQLite pragma %s: %w",statement,err)}
    }
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return err}
    defer tx.Rollback()
    var version int
    if err=tx.QueryRow("PRAGMA user_version").Scan(&version);err!=nil{return err}
    if version>latestSQLiteVersion {return fmt.Errorf("database schema version %d is newer than Regate",version)}
    if version==0 {
        for _,stmt:=range []string{
            `CREATE TABLE IF NOT EXISTS revisions (
                seq INTEGER PRIMARY KEY AUTOINCREMENT,
                id TEXT NOT NULL UNIQUE,
                digest TEXT NOT NULL,
                parent TEXT NOT NULL,
                activated TEXT NOT NULL,
                source TEXT NOT NULL
            )`,
            `CREATE TABLE IF NOT EXISTS drafts (
                id TEXT PRIMARY KEY,
                base TEXT NOT NULL,
                source TEXT NOT NULL,
                created TEXT NOT NULL
            )`,
            `CREATE TABLE IF NOT EXISTS traces (
                seq INTEGER PRIMARY KEY AUTOINCREMENT,
                at TEXT NOT NULL,
                record TEXT NOT NULL
            )`,
            `CREATE INDEX IF NOT EXISTS traces_at ON traces(seq DESC)`,
            `CREATE TABLE IF NOT EXISTS effect_events (
                seq INTEGER PRIMARY KEY AUTOINCREMENT,
                effect_id TEXT NOT NULL,
                status TEXT NOT NULL,
                at TEXT NOT NULL,
                record TEXT NOT NULL
            )`,
            `CREATE INDEX IF NOT EXISTS effect_events_id ON effect_events(effect_id, seq)`,
            `CREATE TABLE IF NOT EXISTS effect_current (
                effect_id TEXT PRIMARY KEY,
                status TEXT NOT NULL,
                seq INTEGER NOT NULL,
                record TEXT NOT NULL
            )`,
            `CREATE TABLE IF NOT EXISTS tasks (
                task_id TEXT PRIMARY KEY,
                status TEXT NOT NULL,
                record TEXT NOT NULL
            )`,
            `CREATE INDEX IF NOT EXISTS tasks_status ON tasks(status)`,
            `CREATE TABLE IF NOT EXISTS suspended (
                run_id TEXT PRIMARY KEY,
                record TEXT NOT NULL
            )`,
            `CREATE TABLE IF NOT EXISTS layouts (
                name TEXT PRIMARY KEY,
                record TEXT NOT NULL
            )`,
            "PRAGMA user_version=1",
        } {
            if _,err:=tx.Exec(stmt);err!=nil{return fmt.Errorf("SQLite schema migration: %w",err)}
        }
    }
    if version<2 {
        if _,err=tx.Exec(`CREATE TABLE IF NOT EXISTS execution_leases (
            name TEXT PRIMARY KEY,
            holder TEXT NOT NULL,
            epoch INTEGER NOT NULL CHECK(epoch > 0),
            deadline_ns INTEGER NOT NULL
        )`);err!=nil{return err}
        if _,err=tx.Exec("PRAGMA user_version=2");err!=nil{return err}
    }
    if version<3 {
        if _,err=tx.Exec(`CREATE TABLE IF NOT EXISTS task_grants (
            task_id TEXT PRIMARY KEY REFERENCES tasks(task_id),
            owner_digest TEXT NOT NULL,
            revision TEXT NOT NULL,
            provider TEXT NOT NULL,
            issuer TEXT NOT NULL,
            audience TEXT NOT NULL,
            scopes_digest TEXT NOT NULL,
            generation INTEGER NOT NULL CHECK(generation > 0),
            expires_ns INTEGER NOT NULL,
            state TEXT NOT NULL CHECK(state IN ('active','refreshing','needs_reauth','revoked')),
            key_id TEXT NOT NULL,
            nonce BLOB NOT NULL,
            ciphertext BLOB NOT NULL,
            updated_ns INTEGER NOT NULL
        )`);err!=nil{return fmt.Errorf("SQLite v3 task_grants migration: %w",err)}
        if _,err=tx.Exec("PRAGMA user_version=3");err!=nil{return err}
    }
    if version<4 {
        if _,err=tx.Exec(`CREATE TABLE IF NOT EXISTS task_queue (
            seq INTEGER PRIMARY KEY AUTOINCREMENT,
            task_id TEXT NOT NULL UNIQUE REFERENCES tasks(task_id) ON DELETE CASCADE,
            owner_digest TEXT NOT NULL,
            revision TEXT NOT NULL,
            surface TEXT NOT NULL,
            tool TEXT NOT NULL,
            key_id TEXT NOT NULL,
            nonce BLOB NOT NULL,
            ciphertext BLOB NOT NULL,
            created_ns INTEGER NOT NULL
        )`);err!=nil{return fmt.Errorf("SQLite v4 task_queue migration: %w",err)}
        if _,err=tx.Exec("PRAGMA user_version=4");err!=nil{return err}
    }
    if version<5 {
        for _,stmt:=range []string{
            `CREATE TABLE IF NOT EXISTS task_queue_turns (
                owner_digest TEXT PRIMARY KEY,
                last_turn INTEGER NOT NULL CHECK(last_turn > 0)
            )`,
            `CREATE TABLE IF NOT EXISTS task_queue_clock (
                name TEXT PRIMARY KEY,
                turn INTEGER NOT NULL CHECK(turn >= 0)
            )`,
            `INSERT OR IGNORE INTO task_queue_clock(name,turn) VALUES('default',0)`,
            `CREATE INDEX IF NOT EXISTS task_queue_owner_seq
                ON task_queue(owner_digest,seq)`,
            "PRAGMA user_version=5",
        }{
            if _,err=tx.Exec(stmt);err!=nil{
                return fmt.Errorf("SQLite v5 fair queue migration: %w",err)
            }
        }
    }
    if version<6 {
        if _,err=tx.Exec(`CREATE TABLE IF NOT EXISTS audit_events (
            seq INTEGER PRIMARY KEY AUTOINCREMENT,
            at TEXT NOT NULL,
            category TEXT NOT NULL,
            action TEXT NOT NULL,
            outcome TEXT NOT NULL,
            prev_digest TEXT NOT NULL,
            digest TEXT NOT NULL
        )`);err!=nil{return fmt.Errorf("SQLite v6 audit migration: %w",err)}
        if _,err=tx.Exec("PRAGMA user_version=6");err!=nil{return err}
    }
    if version < 7 {
        for _, statement := range []string{
            `CREATE TABLE IF NOT EXISTS oauth_quota (
                principal BLOB PRIMARY KEY NOT NULL CHECK (length(principal) = 32),
                ready_ns INTEGER NOT NULL
            ) WITHOUT ROWID`,
            "CREATE INDEX IF NOT EXISTS oauth_quota_ready ON oauth_quota(ready_ns)",
            "PRAGMA user_version=7",
        } {
            if _, err = tx.Exec(statement); err != nil {
                return fmt.Errorf("SQLite v7 OAuth quota migration: %w", err)
            }
        }
    }
    if version<8 {
        if _,err=tx.Exec(`CREATE TABLE IF NOT EXISTS static_config (
            id INTEGER PRIMARY KEY CHECK(id=1),
            prefix TEXT NOT NULL,
            since_seq INTEGER NOT NULL CHECK(since_seq>=0)
        )`);err!=nil{return err}
        if _,err=tx.Exec("INSERT OR IGNORE INTO static_config(id,prefix,since_seq) VALUES(1,'',0)");err!=nil{return err}
        if _,err=tx.Exec("PRAGMA user_version=8");err!=nil{return err}
    }
    if err = s.initializeObservations(tx, version); err != nil { return err }
    // Refuse to open a modified or truncated historical audit chain.
    if err=verifyAuditTx(tx);err!=nil{return fmt.Errorf("audit integrity: %w",err)}
    var count int
    if err=tx.QueryRow("SELECT COUNT(*) FROM revisions").Scan(&count);err!=nil{return err}
    if count==0 {
        found,err:=importLegacy(tx,s.Dir)
        if err!=nil{return fmt.Errorf("legacy migration: %w",err)}
        if !found {
            initial,prefix,err:=bootstrap()
            if err!=nil{return err}
            if initial=="" {return fmt.Errorf("empty initial configuration")}
            if !strings.HasPrefix(initial,prefix) {return ErrStaticConfigConflict}
            revision,err:=activateTx(tx,initial,"")
            if err!=nil{return err}
            if prefix!="" {
                if err=recordStaticConfig(tx,prefix,revision.ID);err!=nil{return err}
            }
        }
    }
    // Inspect the lease and recover within the same write transaction. A
    // worker cannot acquire leadership between the check and recovery.
    live,err:=leaseLiveTx(tx,time.Now().UnixNano())
    if err!=nil{return err}
    if !live {
        if err=recoverDispatches(tx);err!=nil{return err}
        if err=recoverInterruptedTasksTx(tx);err!=nil{return err}
        if err=recoverRefreshingGrantsTx(tx);err!=nil{return err}
    }
    return tx.Commit()
}
func activateTx(tx *sql.Tx,source,parent string)(Revision,error) {
    revision:=Revision{ID:Hash(source+ID()),Digest:Hash(source),Parent:parent,Activated:time.Now().UTC()}
    _,err:=tx.Exec(`INSERT INTO revisions(id,digest,parent,activated,source)
        VALUES(?,?,?,?,?)`,revision.ID,revision.Digest,revision.Parent,revision.Activated.Format(time.RFC3339Nano),source)
    if err!=nil{return Revision{},err}
    return revision,nil
}

func (s *Store) Active()(Revision,string,error) {
    var r Revision
    var activated,source string
    err:=s.db.QueryRow(`SELECT id,digest,parent,activated,source
        FROM revisions ORDER BY seq DESC LIMIT 1`).Scan(&r.ID,&r.Digest,&r.Parent,&activated,&source)
    if errors.Is(err,sql.ErrNoRows) {return Revision{},"",errors.New("no revisions")}
    if err!=nil{return Revision{},"",err}
    if r.Activated,err=time.Parse(time.RFC3339Nano,activated);err!=nil{return Revision{},"",err}
    return r,source,nil
}
func (s *Store) Source(id string)(string,error) {
    if len(id)!=20 {return "",fmt.Errorf("invalid revision id")}
    var source string
    err:=s.db.QueryRow("SELECT source FROM revisions WHERE id=?",id).Scan(&source)
    return source,err
}
func (s *Store) DraftCreate()(Draft,error) {
    return s.createDraft("", "")
}

// DraftCreateFromRevision saves a generated source only if its reviewed
// revision is still active. The draft is inserted with its complete source.
func (s *Store) DraftCreateFromRevision(base, source string) (Draft, error) {
    if base == "" || source == "" { return Draft{}, ErrDraftSourceConflict }
    return s.createDraft(base, source)
}

func (s *Store) createDraft(expected, proposed string) (Draft,error) {
    // Read revision and insert draft within one transaction so activation
    // cannot change the base between these operations in another process.
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Draft{},err}
    defer tx.Rollback()
    var base,source string
    err=tx.QueryRow("SELECT id,source FROM revisions ORDER BY seq DESC LIMIT 1").Scan(&base,&source)
    if err!=nil{return Draft{},err}
    if expected != "" {
        if base != expected { return Draft{}, ErrDraftSourceConflict }
        if err := checkStaticSource(tx, proposed); err != nil { return Draft{}, err }
        source = proposed
    }
    draft:=Draft{ID:ID(),Base:base,Source:source,SourceDigest:SourceDigest(source),Created:time.Now().UTC()}
    if _,err=tx.Exec("INSERT INTO drafts(id,base,source,created) VALUES(?,?,?,?)",
        draft.ID,draft.Base,draft.Source,draft.Created.Format(time.RFC3339Nano));err!=nil{return Draft{},err}
    return draft,tx.Commit()
}
func (s *Store) DraftGet(id string)(Draft,error) {
    if len(id)!=24{return Draft{},fmt.Errorf("invalid draft id")}
    var d Draft
    var date string
    err:=s.db.QueryRow("SELECT id,base,source,created FROM drafts WHERE id=?",id).
        Scan(&d.ID,&d.Base,&d.Source,&date)
    if err!=nil{return Draft{},err}
    d.Created,err=time.Parse(time.RFC3339Nano,date)
    if err != nil { return Draft{},err }
    d.SourceDigest = SourceDigest(d.Source)
    return d,nil
}
func (s *Store) DraftUpdate(id,source string)(Draft,error) {
    if len(id)!=24{return Draft{},fmt.Errorf("invalid draft id")}
    result,err:=s.db.Exec("UPDATE drafts SET source=? WHERE id=?",source,id)
    if err!=nil{return Draft{},err}
    rows,err:=result.RowsAffected()
    if err!=nil{return Draft{},err}
    if rows==0{return Draft{},sql.ErrNoRows}
    return s.DraftGet(id)
}
// DraftUpdateIfSource atomically changes a draft only if its persisted
// source is exactly the snapshot the editor read. This prevents concurrent
// writers, including independent Regate processes, from losing an edit.
func (s *Store) DraftUpdateIfSource(id, expectedSource, source string)(Draft,error) {
    if len(id)!=24 { return Draft{},fmt.Errorf("invalid draft id") }
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil {return Draft{},err}
    defer tx.Rollback()
    result,err:=tx.Exec("UPDATE drafts SET source=? WHERE id=? AND source=?",source,id,expectedSource)
    if err!=nil { return Draft{},err }
    rows,err:=result.RowsAffected()
    if err!=nil { return Draft{},err }
    if rows==0 {
        var count int
        if err:=tx.QueryRow("SELECT count(*) FROM drafts WHERE id=?",id).Scan(&count);err!=nil {
            return Draft{},err
        }
        if count==0 {return Draft{},sql.ErrNoRows}
        return Draft{},ErrDraftSourceConflict
    }
    var base,date string
    if err:=tx.QueryRow("SELECT base,created FROM drafts WHERE id=?",id).Scan(&base,&date);err!=nil {
        return Draft{},err
    }
    created,err:=time.Parse(time.RFC3339Nano,date)
    if err!=nil {return Draft{},err}
    if err=tx.Commit();err!=nil {return Draft{},err}
    return Draft{ID:id,Base:base,Source:source,SourceDigest:SourceDigest(source),Created:created},nil
}
// ActivateIfSource pins the exact HCL bytes validated and prepared by the
// service. A concurrent edit must never activate different bytes from those
// used to construct the public MCP catalog and authorization policies.
func (s *Store) ActivateIfSource(id, expectedSource string)(Revision,error) {
    return s.activateDraft(id,&expectedSource)
}
func (s *Store) Activate(id string)(Revision,error) {
    return s.activateDraft(id,nil)
}
func (s *Store) activateDraft(id string, expectedSource *string)(Revision,error) {
    if len(id)!=24{return Revision{},fmt.Errorf("invalid draft id")}
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Revision{},err}
    defer tx.Rollback()
    if err := s.checkExecutionLeaseTx(tx); err != nil {
        return Revision{}, err
    }
    var base,source,active string
    err=tx.QueryRow("SELECT base,source FROM drafts WHERE id=?",id).Scan(&base,&source)
    if err!=nil{return Revision{},err}
    if expectedSource!=nil && source!=*expectedSource {
        return Revision{},ErrDraftSourceConflict
    }
    err=tx.QueryRow("SELECT id FROM revisions ORDER BY seq DESC LIMIT 1").Scan(&active)
    if err!=nil{return Revision{},err}
    if active!=base {
        return Revision{},fmt.Errorf("revision conflict: draft based on %s, current %s",base,active)
    }
    if err=checkStaticSource(tx,source);err!=nil{return Revision{},err}
    rev,err:=activateTx(tx,source,active)
    if err!=nil{return Revision{},err}
    if _,err=tx.Exec("DELETE FROM drafts WHERE id=?",id);err!=nil{return Revision{},err}
    return rev,tx.Commit()
}
func (s *Store) Rollback(target string)(Revision,error) {
    if len(target)!=20{return Revision{},fmt.Errorf("invalid revision id")}
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Revision{},err}
    defer tx.Rollback()
    if err := s.checkExecutionLeaseTx(tx); err != nil {
        return Revision{}, err
    }
    var source,parent string
    var seq, since int64
    var prefix string
    err=tx.QueryRow(`SELECT revisions.source, revisions.seq, static_config.prefix, static_config.since_seq
        FROM revisions CROSS JOIN static_config WHERE revisions.id=? AND static_config.id=1`,target).
        Scan(&source,&seq,&prefix,&since)
    if err!=nil{return Revision{},err}
    if seq<since || !strings.HasPrefix(source,prefix) {return Revision{},ErrStaticConfigConflict}
    err=tx.QueryRow("SELECT id FROM revisions ORDER BY seq DESC LIMIT 1").Scan(&parent)
    if err!=nil{return Revision{},err}
    rev,err:=activateTx(tx,source,parent)
    if err!=nil{return Revision{},err}
    return rev,tx.Commit()
}
func (s *Store) History() []Revision {
    rows,err:=s.db.Query("SELECT id,digest,parent,activated FROM revisions ORDER BY seq")
    if err!=nil{return []Revision{}}
    defer rows.Close()
    revisions:=[]Revision{}
    for rows.Next() {
        var r Revision
        var timeText string
        if rows.Scan(&r.ID,&r.Digest,&r.Parent,&timeText)!=nil{break}
        r.Activated,err=time.Parse(time.RFC3339Nano,timeText)
        if err!=nil{break}
        revisions=append(revisions,r)
    }
    return revisions
}
func (s *Store) WriteTrace(t Trace) {
    if t.At.IsZero(){t.At=time.Now().UTC()}
    // Store only bounded, sanitized diagnostics, even when a new caller
    // forgets the runtime's own error redaction. This boundary applies to
    // durable SQLite records, not only to HTTP responses.
    t.Message=expr.RedactDiagnostic(t.Message)
    raw,err:=json.Marshal(t)
    if err!=nil{return}
    _,_=s.db.Exec("INSERT INTO traces(at,record) VALUES(?,?)",
        t.At.Format(time.RFC3339Nano),string(raw))
    // Keep an intentionally bounded projection as in the pre-SQLite store.
    _,_=s.db.Exec(`DELETE FROM traces WHERE seq NOT IN
        (SELECT seq FROM traces ORDER BY seq DESC LIMIT 2000)`)
}
// TraceQuery returns an exact-match, newest-first view of the retained
// diagnostic projection. Limit applies AFTER filtering so a later matching
// run is not hidden by unrelated newer events. The physical table retains
// only the latest 2000 records; this is not a durable long-term audit feed.
type TraceQuery struct {
    RunID string
    Status string
    Limit int
}

const maxRetainedTraces = 2000

func (s *Store) Traces() []Trace {
    return s.TracesFiltered(TraceQuery{Limit:maxRetainedTraces})
}

func (s *Store) TracesFiltered(query TraceQuery) []Trace {
    limit:=query.Limit
    if limit<=0 || limit>maxRetainedTraces {limit=maxRetainedTraces}
    rows,err:=s.db.Query("SELECT record FROM traces ORDER BY seq DESC LIMIT ?",maxRetainedTraces)
    if err!=nil{return []Trace{}}
    defer rows.Close()
    out:=make([]Trace,0,limit)
    for rows.Next() {
        var raw string
        var t Trace
        if rows.Scan(&raw)!=nil || json.Unmarshal([]byte(raw),&t)!=nil {continue}
        if query.RunID!="" && t.RunID!=query.RunID {continue}
        if query.Status!="" && t.Status!=query.Status {continue}
        out=append(out,t)
        if len(out)>=limit {break}
    }
    return out
}

func writeEffectTx(tx *sql.Tx,event Effect) error {
    if event.ID=="" || event.Status=="" {return fmt.Errorf("effect id and status required")}
    if event.At.IsZero(){event.At=time.Now().UTC()}
    // Free-form diagnostic and operator evidence text is not a credential
    // vault. Preserve effect identity, resource/evidence digests and state,
    // while ensuring new journal records cannot persist raw bearer tokens
    // or obvious PII from an untrusted provider error.
    event.Reason=expr.RedactDiagnostic(event.Reason)
    if event.Resolution!=nil {
        resolution:=*event.Resolution
        resolution.Reason=expr.RedactDiagnostic(resolution.Reason)
        event.Resolution=&resolution
    }
    raw,err:=json.Marshal(event)
    if err!=nil{return err}
    result,err:=tx.Exec(`INSERT INTO effect_events(effect_id,status,at,record)
        VALUES(?,?,?,?)`,event.ID,event.Status,event.At.Format(time.RFC3339Nano),string(raw))
    if err!=nil{return err}
    seq,err:=result.LastInsertId()
    if err!=nil{return err}
    _,err=tx.Exec(`INSERT INTO effect_current(effect_id,status,seq,record) VALUES(?,?,?,?)
        ON CONFLICT(effect_id) DO UPDATE SET status=excluded.status,
          seq=excluded.seq, record=excluded.record`,event.ID,event.Status,seq,string(raw))
    return err
}
func (s *Store) Effect(event Effect) error {
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return err}
    defer tx.Rollback()
    if err=writeEffectTx(tx,event);err!=nil{return err}
    return tx.Commit()
}
func (s *Store) Effects() []Effect {
    rows,err:=s.db.Query("SELECT record FROM effect_current ORDER BY seq DESC")
    if err!=nil{return []Effect{}}
    defer rows.Close()
    events:=[]Effect{}
    for rows.Next() {
        var raw string
        var event Effect
        if rows.Scan(&raw)==nil && json.Unmarshal([]byte(raw),&event)==nil {
            events=append(events,event)
        }
    }
    return events
}
func recoverDispatches(tx *sql.Tx)error {
    rows,err:=tx.Query("SELECT record FROM effect_current WHERE status='dispatching'")
    if err!=nil{return err}
    unknown:=[]Effect{}
    for rows.Next(){
        var raw string
        if err=rows.Scan(&raw);err!=nil{break}
        var item Effect
        if err=json.Unmarshal([]byte(raw),&item);err!=nil{break}
        item.Status="unknown"
        item.Reason="process restarted before effect confirmation; verify external outcome"
        item.At=time.Now().UTC()
        unknown=append(unknown,item)
    }
    if closeErr:=rows.Close();err==nil{err=closeErr}
    if err!=nil{return err}
    for _,item:=range unknown {if err=writeEffectTx(tx,item);err!=nil{return err}}
    return nil
}
func (s *Store) PutSuspended(id string,value any)error {
    if len(id)!=24{return fmt.Errorf("invalid run id")}
    data,err:=json.Marshal(value)
    if err!=nil{return err}
    _,err=s.db.Exec(`INSERT INTO suspended(run_id,record) VALUES(?,?)
        ON CONFLICT(run_id) DO UPDATE SET record=excluded.record`,id,string(data))
    return err
}
func (s *Store) GetSuspended(id string,out any)error {
    if len(id)!=24{return fmt.Errorf("invalid run id")}
    var raw string
    if err:=s.db.QueryRow("SELECT record FROM suspended WHERE run_id=?",id).Scan(&raw);err!=nil{return err}
    return json.Unmarshal([]byte(raw),out)
}
func (s *Store) DeleteSuspended(id string)error {
    if len(id)!=24{return fmt.Errorf("invalid run id")}
    _,err:=s.db.Exec("DELETE FROM suspended WHERE run_id=?",id)
    return err
}
func (s *Store) Layout(name string)(map[string]any,error) {
    if !safeName(name){return nil,fmt.Errorf("invalid layout name")}
    var raw string
    err:=s.db.QueryRow("SELECT record FROM layouts WHERE name=?",name).Scan(&raw)
    if errors.Is(err,sql.ErrNoRows){return map[string]any{},nil}
    if err!=nil{return nil,err}
    var value map[string]any
    if err=json.Unmarshal([]byte(raw),&value);err!=nil{return nil,err}
    return value,nil
}
func (s *Store) SaveLayout(name string,value map[string]any)error {
    if !safeName(name){return fmt.Errorf("invalid layout name")}
    raw,err:=json.Marshal(value)
    if err!=nil{return err}
    _,err=s.db.Exec(`INSERT INTO layouts(name,record) VALUES(?,?)
        ON CONFLICT(name) DO UPDATE SET record=excluded.record`,name,string(raw))
    return err
}
func safeName(s string)bool {
    if s==""{return false}
    for _,r:=range s {
        if !(r>='a'&&r<='z'||r>='A'&&r<='Z'||r>='0'&&r<='9'||r=='_'||r=='-'){return false}
    }
    return true
}

// Backup retains the established simple API while publishing only a
// verified, complete snapshot with no-overwrite semantics.
func (s *Store) Backup(destination string)error {
    _,err:=s.BackupVerified(destination)
    return err
}

// SortEffects provides deterministic ordering for model checking and
// reconciliation tooling independent of SQLite cursor collation.
func SortEffects(items []Effect){
    sort.Slice(items,func(i,j int)bool{
        if items[i].At.Equal(items[j].At){return items[i].ID<items[j].ID}
        return items[i].At.Before(items[j].At)
    })
}
