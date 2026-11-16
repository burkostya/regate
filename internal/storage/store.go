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
)

func ID() string {
    b := make([]byte,12)
    if _,err:=rand.Read(b);err!=nil {panic("secure random generator failed: "+err.Error())}
    return hex.EncodeToString(b)
}
func Hash(s string) string {h:=sha256.Sum256([]byte(s));return hex.EncodeToString(h[:])[:20]}

type Draft struct {
    ID string `json:"id"`
    Base string `json:"base"`
    Source string `json:"source"`
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
    Status string `json:"status"`
    At time.Time `json:"at"`
    Reason string `json:"reason,omitempty"`
    Resolution *EffectResolution `json:"resolution,omitempty"`
}

// The single-connection pool makes all store-level operations serial without
// requiring CGO. Cross-process writers are coordinated by SQLite's WAL and
// BEGIN IMMEDIATE (configured in the driver DSN), not by an in-memory mutex.
type Store struct {
    Dir string
    db *sql.DB
    mu sync.RWMutex
}
const databaseName = "regate.sqlite"

func New(dir,initial string) (*Store,error) {
    if err:=os.MkdirAll(dir,0700);err!=nil{return nil,err}
    path:=filepath.Join(dir,databaseName)
    uri:=(&url.URL{Scheme:"file",Path:path}).String()+
        "?_txlock=immediate&_pragma=busy_timeout(15000)"
    db,err:=sql.Open("sqlite3",uri)
    if err!=nil{return nil,err}
    db.SetMaxOpenConns(1)
    db.SetMaxIdleConns(1)
    s:=&Store{Dir:dir,db:db}
    if err=s.initialize(initial);err!=nil {
        _=db.Close()
        return nil,err
    }
    // A previously running task has no durable execution lease after a
    // process crash. Never restart it blindly: its external outcome may
    // require reconciliation. The effect log is recovered independently.
    if err=s.RecoverInterruptedTasks();err!=nil {
        _=db.Close()
        return nil,err
    }
    return s,nil
}
func (s *Store) Close() error {return s.db.Close()}

func (s *Store) initialize(initial string) error {
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
    if version>1 {return fmt.Errorf("database schema version %d is newer than Regate",version)}
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
    var count int
    if err=tx.QueryRow("SELECT COUNT(*) FROM revisions").Scan(&count);err!=nil{return err}
    if count==0 {
        found,err:=importLegacy(tx,s.Dir)
        if err!=nil{return fmt.Errorf("legacy migration: %w",err)}
        if !found {
            if initial=="" {return fmt.Errorf("empty initial configuration")}
            if _,err=activateTx(tx,initial,"");err!=nil{return err}
        }
    }
    // Crash during a non-idempotent dispatch does NOT mean it failed. Preserve
    // the uncertain effect; external read-back or an operator must resolve it.
    if err=recoverDispatches(tx);err!=nil{return err}
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
    // Read revision and insert draft within one transaction so activation
    // cannot change the base between these operations in another process.
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Draft{},err}
    defer tx.Rollback()
    var base,source string
    err=tx.QueryRow("SELECT id,source FROM revisions ORDER BY seq DESC LIMIT 1").Scan(&base,&source)
    if err!=nil{return Draft{},err}
    draft:=Draft{ID:ID(),Base:base,Source:source,Created:time.Now().UTC()}
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
    return d,err
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
func (s *Store) Activate(id string)(Revision,error) {
    if len(id)!=24{return Revision{},fmt.Errorf("invalid draft id")}
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return Revision{},err}
    defer tx.Rollback()
    var base,source,active string
    err=tx.QueryRow("SELECT base,source FROM drafts WHERE id=?",id).Scan(&base,&source)
    if err!=nil{return Revision{},err}
    err=tx.QueryRow("SELECT id FROM revisions ORDER BY seq DESC LIMIT 1").Scan(&active)
    if err!=nil{return Revision{},err}
    if active!=base {
        return Revision{},fmt.Errorf("revision conflict: draft based on %s, current %s",base,active)
    }
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
    var source,parent string
    err=tx.QueryRow("SELECT source FROM revisions WHERE id=?",target).Scan(&source)
    if err!=nil{return Revision{},err}
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
    raw,err:=json.Marshal(t)
    if err!=nil{return}
    _,_=s.db.Exec("INSERT INTO traces(at,record) VALUES(?,?)",
        t.At.Format(time.RFC3339Nano),string(raw))
    // Keep an intentionally bounded projection as in the pre-SQLite store.
    _,_=s.db.Exec(`DELETE FROM traces WHERE seq NOT IN
        (SELECT seq FROM traces ORDER BY seq DESC LIMIT 2000)`)
}
func (s *Store) Traces() []Trace {
    rows,err:=s.db.Query("SELECT record FROM traces ORDER BY seq DESC LIMIT 2000")
    if err!=nil{return []Trace{}}
    defer rows.Close()
    out:=[]Trace{}
    for rows.Next(){
        var raw string
        var t Trace
        if rows.Scan(&raw)==nil && json.Unmarshal([]byte(raw),&t)==nil {out=append(out,t)}
    }
    return out
}

func writeEffectTx(tx *sql.Tx,event Effect) error {
    if event.ID=="" || event.Status=="" {return fmt.Errorf("effect id and status required")}
    if event.At.IsZero(){event.At=time.Now().UTC()}
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

// Backup creates a consistent standalone SQLite snapshot without copying an
// in-use WAL file. Its destination must not exist (no implicit overwrite).
func (s *Store) Backup(destination string)error {
    if strings.TrimSpace(destination)==""{return fmt.Errorf("empty backup path")}
    absolute,err:=filepath.Abs(destination)
    if err!=nil{return err}
    if _,err=os.Stat(absolute);err==nil{return fmt.Errorf("backup already exists")}
    if !errors.Is(err,os.ErrNotExist){return err}
    if _,err=s.db.Exec("VACUUM INTO ?",absolute);err!=nil{return err}
    return os.Chmod(absolute,0600)
}

// SortEffects provides deterministic ordering for model checking and
// reconciliation tooling independent of SQLite cursor collation.
func SortEffects(items []Effect){
    sort.Slice(items,func(i,j int)bool{
        if items[i].At.Equal(items[j].At){return items[i].ID<items[j].ID}
        return items[i].At.Before(items[j].At)
    })
}
