package storage

// SQLite is the sole authoritative store after startup. The old JSON/HCL files
// are read only once during a transactionally committed migration and retained
// as an operator-readable backup; they are never written by the new backend.
import (
    "bufio"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "strings"
    "time"

    _ "github.com/ncruces/go-sqlite3/driver"
)

const currentSchemaVersion = 1

func openSQLite(dir, initial string) (_ *Store, err error) {
    if err = os.MkdirAll(dir,0700);err!=nil{return nil,err}
    file:=filepath.Join(dir,"regate.sqlite3")
    db,err:=sql.Open("sqlite3",file+"?_txlock=immediate")
    if err!=nil{return nil,err}
    db.SetMaxOpenConns(1) // deterministic single writer with a bounded queue
    db.SetMaxIdleConns(1)
    fail:=func(e error)(*Store,error) { _=db.Close(); return nil,e }
    for _,stmt:=range []string{
        "PRAGMA journal_mode=WAL",
        "PRAGMA synchronous=FULL",
        "PRAGMA busy_timeout=5000",
        "PRAGMA foreign_keys=ON",
    } {
        if _,e:=db.Exec(stmt);e!=nil{return fail(fmt.Errorf("SQLite startup %s: %w",stmt,e))}
    }
    if e:=os.Chmod(file,0600);e!=nil{return fail(e)}
    s:=&Store{Dir:dir,db:db}
    if e:=s.initSQLite(initial);e!=nil{return fail(e)}
    return s,nil
}

func (s *Store) Close() error {
    if s.db==nil {return nil}
    return s.db.Close()
}

func (s *Store) initSQLite(initial string) error {
    tx,err:=s.db.Begin()
    if err!=nil{return err}
    defer tx.Rollback()
    var version int
    if err:=tx.QueryRow("PRAGMA user_version").Scan(&version);err!=nil{return err}
    if version>currentSchemaVersion{return fmt.Errorf("SQLite schema %d newer than Regate %d",version,currentSchemaVersion)}
    for _,statement:=range []string{
        "CREATE TABLE IF NOT EXISTS revisions (id TEXT PRIMARY KEY, digest TEXT NOT NULL, parent TEXT NOT NULL, activated_ns INTEGER NOT NULL, source TEXT NOT NULL)",
        "CREATE TABLE IF NOT EXISTS state (singleton INTEGER PRIMARY KEY CHECK(singleton=1), active TEXT NOT NULL REFERENCES revisions(id))",
        "CREATE TABLE IF NOT EXISTS drafts (id TEXT PRIMARY KEY, base TEXT NOT NULL REFERENCES revisions(id), source TEXT NOT NULL, created_ns INTEGER NOT NULL)",
        "CREATE TABLE IF NOT EXISTS traces (seq INTEGER PRIMARY KEY AUTOINCREMENT, at_ns INTEGER NOT NULL, payload BLOB NOT NULL)",
        "CREATE INDEX IF NOT EXISTS traces_by_time ON traces(at_ns DESC, seq DESC)",
        "CREATE TABLE IF NOT EXISTS effects (seq INTEGER PRIMARY KEY AUTOINCREMENT, effect_id TEXT NOT NULL, status TEXT NOT NULL, at_ns INTEGER NOT NULL, payload BLOB NOT NULL)",
        "CREATE INDEX IF NOT EXISTS effects_by_id ON effects(effect_id,seq DESC)",
        "CREATE TABLE IF NOT EXISTS suspended (id TEXT PRIMARY KEY, payload BLOB NOT NULL)",
        "CREATE TABLE IF NOT EXISTS layouts (name TEXT PRIMARY KEY, payload BLOB NOT NULL)",
        "CREATE TABLE IF NOT EXISTS tasks (id TEXT PRIMARY KEY, status TEXT NOT NULL, payload BLOB NOT NULL)",
    } {
        if _,err=tx.Exec(statement);err!=nil{return fmt.Errorf("create SQLite schema: %w",err)}
    }
    var count int
    if err=tx.QueryRow("SELECT COUNT(*) FROM revisions").Scan(&count);err!=nil{return err}
    if count==0 {
        legacy,err:=s.importLegacy(tx)
        if err!=nil{return fmt.Errorf("migrate existing Regate state: %w",err)}
        if !legacy {
            if _,err=s.appendRevision(tx,initial,"");err!=nil{return err}
        }
    }
    if _,err=tx.Exec(fmt.Sprintf("PRAGMA user_version=%d",currentSchemaVersion));err!=nil{return err}
    if err=tx.Commit();err!=nil{return err}
    // Dispatching was fsynced but no final response was durably recorded. We
    // must conservatively convert it to UNKNOWN after a crash, never replay it.
    return s.recoverSQLite()
}

func (s *Store) appendRevision(tx *sql.Tx, source, parent string) (Revision,error) {
    rev:=Revision{ID:Hash(source+ID()),Digest:Hash(source),Parent:parent,Activated:time.Now().UTC()}
    if _,err:=tx.Exec("INSERT INTO revisions(id,digest,parent,activated_ns,source) VALUES (?,?,?,?,?)",
        rev.ID,rev.Digest,rev.Parent,rev.Activated.UnixNano(),source);err!=nil {return Revision{},err}
    if _,err:=tx.Exec("INSERT INTO state(singleton,active) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET active=excluded.active",
        rev.ID);err!=nil{return Revision{},err}
    return rev,nil
}

// Import JSONL only after a validated revision history is available. Corrupt
// *trailing* diagnostic entries are ignored (as in the legacy store), while
// critical HCL/revision/task/draft data rejects the entire migration.
func (s *Store) importLegacy(tx *sql.Tx) (bool,error) {
    historyBlob,err:=os.ReadFile(filepath.Join(s.Dir,"history.json"))
    if errors.Is(err,os.ErrNotExist) {return false,nil}
    if err!=nil{return false,err}
    var history []Revision
    if err:=json.Unmarshal(historyBlob,&history);err!=nil{return false,err}
    if len(history)==0{return false,errors.New("legacy history contains zero revisions")}
    for i,rev:=range history {
        if len(rev.ID)!=20 || (i>0 && rev.Parent!=history[i-1].ID) {return false,fmt.Errorf("invalid legacy revision chain at %d",i)}
        source,err:=os.ReadFile(filepath.Join(s.Dir,"revisions",rev.ID+".hcl"))
        if err!=nil{return false,err}
        if Hash(string(source))!=rev.Digest {return false,fmt.Errorf("legacy revision %s digest mismatch",rev.ID)}
        if _,err=tx.Exec("INSERT INTO revisions(id,digest,parent,activated_ns,source) VALUES(?,?,?,?,?)",
            rev.ID,rev.Digest,rev.Parent,rev.Activated.UnixNano(),string(source));err!=nil{return false,err}
    }
    if _,err=tx.Exec("INSERT INTO state(singleton,active) VALUES(1,?)",history[len(history)-1].ID);err!=nil{return false,err}
    // Drafts may contain unsupported HCL deliberately; preserve the raw text.
    if err=s.importJSONDir("drafts",func(name string,data []byte)error{
        var d Draft
        if err:=json.Unmarshal(data,&d);err!=nil{return err}
        if d.ID!=strings.TrimSuffix(name,".json") || len(d.ID)!=24{return errors.New("malformed legacy draft")}
        _,err:=tx.Exec("INSERT INTO drafts(id,base,source,created_ns) VALUES(?,?,?,?)",
            d.ID,d.Base,d.Source,d.Created.UnixNano())
        return err
    });err!=nil{return false,err}
    if err=s.importJSONDir("tasks",func(name string,data []byte)error{
        var task TaskRecord
        if err:=json.Unmarshal(data,&task);err!=nil{return err}
        if task.TaskID!=strings.TrimSuffix(name,".json") || len(task.TaskID)!=24{return errors.New("malformed legacy task")}
        _,err:=tx.Exec("INSERT INTO tasks(id,status,payload) VALUES(?,?,?)",task.TaskID,task.Status,data)
        return err
    });err!=nil{return false,err}
    if err=s.importJSONDir("suspended",func(name string,data []byte)error{
        id:=strings.TrimSuffix(name,".json")
        if len(id)!=24 || !json.Valid(data) {return errors.New("invalid legacy continuation")}
        _,err:=tx.Exec("INSERT INTO suspended(id,payload) VALUES(?,?)",id,data)
        return err
    });err!=nil{return false,err}
    if err=s.importJSONDir("layouts",func(name string,data []byte)error{
        id:=strings.TrimSuffix(name,".json")
        if !safeName(id) || !json.Valid(data) {return errors.New("invalid legacy layout")}
        _,err:=tx.Exec("INSERT INTO layouts(name,payload) VALUES(?,?)",id,data)
        return err
    });err!=nil{return false,err}
    if err=importLegacyLines(filepath.Join(s.Dir,"traces.jsonl"),func(data []byte)error{
        var record Trace
        if json.Unmarshal(data,&record)!=nil || record.RunID=="" {return nil}
        _,err:=tx.Exec("INSERT INTO traces(at_ns,payload) VALUES(?,?)",record.At.UnixNano(),data)
        return err
    });err!=nil{return false,err}
    if err=importLegacyLines(filepath.Join(s.Dir,"effects.jsonl"),func(data []byte)error{
        var record Effect
        if json.Unmarshal(data,&record)!=nil || record.ID=="" {return nil}
        _,err:=tx.Exec("INSERT INTO effects(effect_id,status,at_ns,payload) VALUES(?,?,?,?)",
            record.ID,record.Status,record.At.UnixNano(),data)
        return err
    });err!=nil{return false,err}
    return true,nil
}

func (s *Store) importJSONDir(subdir string,insert func(string,[]byte)error) error {
    names,err:=os.ReadDir(filepath.Join(s.Dir,subdir))
    if errors.Is(err,os.ErrNotExist){return nil}
    if err!=nil{return err}
    for _,entry:=range names {
        if entry.IsDir() || !strings.HasSuffix(entry.Name(),".json") {continue}
        if !safeName(strings.TrimSuffix(entry.Name(),".json")) {return fmt.Errorf("invalid legacy %s entry name",subdir)}
        data,err:=os.ReadFile(filepath.Join(s.Dir,subdir,entry.Name()))
        if err!=nil{return err}
        if err=insert(entry.Name(),data);err!=nil{return fmt.Errorf("%s/%s: %w",subdir,entry.Name(),err)}
    }
    return nil
}
func importLegacyLines(path string,consume func([]byte)error) error {
    file,err:=os.Open(path)
    if errors.Is(err,os.ErrNotExist){return nil}
    if err!=nil{return err}
    defer file.Close()
    scanner:=bufio.NewScanner(file)
    scanner.Buffer(make([]byte,4096),8<<20)
    for scanner.Scan() {
        line:=scanner.Bytes()
        if !json.Valid(line) {continue}
        if err=consume(line);err!=nil{return err}
    }
    return scanner.Err()
}

// Backup captures a consistent online snapshot while writers continue. The
// source remains WAL-backed; VACUUM INTO creates a standalone SQLite database.
func (s *Store) Backup(destination string) error {
    if s.db==nil {return errors.New("SQLite is not open")}
    if destination=="" {return errors.New("backup destination required")}
    absolute,err:=filepath.Abs(destination)
    if err!=nil{return err}
    source,err:=filepath.Abs(filepath.Join(s.Dir,"regate.sqlite3"))
    if err!=nil{return err}
    if absolute==source{return errors.New("backup cannot overwrite active database")}
    if _,err=os.Stat(absolute);err==nil {return errors.New("backup destination already exists")}
    if !errors.Is(err,os.ErrNotExist){return err}
    if _,err=s.db.Exec("VACUUM INTO ?",absolute);err!=nil {return err}
    return os.Chmod(absolute,0600)
}

// Avoid unused imports when files in this package are partially migrated.
var _ = io.EOF
