package storage

import (
    "bufio"
    "database/sql"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "path/filepath"
    "strings"
    "time"
)

// importLegacy is run only when SQLite has no committed revision. All
// imported entities share the caller's transaction. Legacy files are never
// removed, modified or renamed; they remain an operator-managed backup.
//
// Refuse malformed or missing referenced data rather than silently replacing
// a valid old state with the supplied "initial" configuration.
func importLegacy(tx *sql.Tx,dir string)(bool,error) {
    raw,err:=os.ReadFile(filepath.Join(dir,"history.json"))
    if errors.Is(err,os.ErrNotExist) {
        artifacts,err:=legacyArtifactsPresent(dir)
        if err!=nil{return false,err}
        if artifacts {return false,fmt.Errorf("legacy artifacts exist without history.json")}
        return false,nil
    }
    if err!=nil{return false,err}
    var history []Revision
    if err=json.Unmarshal(raw,&history);err!=nil{return false,fmt.Errorf("history.json: %w",err)}
    if len(history)==0 {return false,fmt.Errorf("legacy history.json has no active revision")}
    known:=map[string]bool{}
    var prior string
    for index,rev:=range history {
        if len(rev.ID)!=20 || rev.Digest=="" || known[rev.ID] || rev.Parent!=prior || rev.Activated.IsZero() {
            return false,fmt.Errorf("legacy revision %d has inconsistent identity or parent",index)
        }
        source,err:=os.ReadFile(filepath.Join(dir,"revisions",rev.ID+".hcl"))
        if err!=nil{return false,fmt.Errorf("legacy revision %s: %w",rev.ID,err)}
        if rev.Digest!=Hash(string(source)){
            return false,fmt.Errorf("legacy revision %s digest mismatch",rev.ID)
        }
        if _,err=tx.Exec(`INSERT INTO revisions(id,digest,parent,activated,source)
            VALUES(?,?,?,?,?)`,rev.ID,rev.Digest,rev.Parent,rev.Activated.UTC().Format(time.RFC3339Nano),string(source));err!=nil {
            return false,fmt.Errorf("import revision %s: %w",rev.ID,err)
        }
        known[rev.ID]=true
        prior=rev.ID
    }
    if err=importJSONDir(tx,dir,"drafts",func(name string,blob []byte)error {
        var draft Draft
        if err:=json.Unmarshal(blob,&draft);err!=nil{return err}
        if len(draft.ID)!=24 || name!=draft.ID || !known[draft.Base] {
            return fmt.Errorf("draft has invalid id/base")
        }
        _,err:=tx.Exec("INSERT INTO drafts(id,base,source,created) VALUES(?,?,?,?)",
            draft.ID,draft.Base,draft.Source,draft.Created.UTC().Format(time.RFC3339Nano))
        return err
    });err!=nil{return false,err}
    if err=importJSONDir(tx,dir,"tasks",func(name string,blob []byte)error {
        var task TaskRecord
        if err:=json.Unmarshal(blob,&task);err!=nil{return err}
        if task.TaskID!=name || len(task.TaskID)!=24 || !known[task.Revision] {
            return fmt.Errorf("task has invalid ID or unknown revision")
        }
        _,err:=tx.Exec("INSERT INTO tasks(task_id,status,record) VALUES(?,?,?)",
            task.TaskID,task.Status,string(blob))
        return err
    });err!=nil{return false,err}
    if err=importJSONDir(tx,dir,"suspended",func(name string,blob []byte)error {
        if len(name)!=24 {return fmt.Errorf("invalid suspended run ID")}
        if !json.Valid(blob) {return fmt.Errorf("invalid suspended checkpoint JSON")}
        _,err:=tx.Exec("INSERT INTO suspended(run_id,record) VALUES(?,?)",name,string(blob))
        return err
    });err!=nil{return false,err}
    if err=importJSONDir(tx,dir,"layouts",func(name string,blob []byte)error {
        if !safeName(name) {return fmt.Errorf("invalid layout name")}
        var layout map[string]any
        if err:=json.Unmarshal(blob,&layout);err!=nil{return err}
        if layout==nil{return fmt.Errorf("invalid layout content")}
        _,err:=tx.Exec("INSERT INTO layouts(name,record) VALUES(?,?)",name,string(blob))
        return err
    });err!=nil{return false,err}
    if err=scanLegacyJSONL(filepath.Join(dir,"traces.jsonl"),func(blob []byte)error{
        var trace Trace
        if err:=json.Unmarshal(blob,&trace);err!=nil{return err}
        if trace.RunID=="" {return fmt.Errorf("trace lacks run_id")}
        _,err:=tx.Exec("INSERT INTO traces(at,record) VALUES(?,?)",
            trace.At.UTC().Format(time.RFC3339Nano),string(blob))
        return err
    });err!=nil{return false,err}
    if err=scanLegacyJSONL(filepath.Join(dir,"effects.jsonl"),func(blob []byte)error{
        var effect Effect
        if err:=json.Unmarshal(blob,&effect);err!=nil{return err}
        return writeEffectTx(tx,effect)
    });err!=nil{return false,err}
    return true,nil
}

func legacyArtifactsPresent(dir string)(bool,error) {
    for _,name:=range []string{"revisions","drafts","tasks","layouts","suspended"} {
        files,err:=os.ReadDir(filepath.Join(dir,name))
        if errors.Is(err,os.ErrNotExist){continue}
        if err!=nil{return false,err}
        for _,file:=range files {
            if !file.IsDir() && !strings.HasSuffix(file.Name(),".tmp"){return true,nil}
        }
    }
    for _,name:=range []string{"traces.jsonl","effects.jsonl"} {
        info,err:=os.Stat(filepath.Join(dir,name))
        if errors.Is(err,os.ErrNotExist){continue}
        if err!=nil{return false,err}
        if info.Size()>0{return true,nil}
    }
    return false,nil
}

func importJSONDir(tx *sql.Tx,dir,folder string,consume func(string,[]byte)error)error {
    _=tx
    entries,err:=os.ReadDir(filepath.Join(dir,folder))
    if errors.Is(err,os.ErrNotExist){return nil}
    if err!=nil{return err}
    for _,entry:=range entries {
        if entry.IsDir() || strings.HasSuffix(entry.Name(),".tmp"){continue}
        if !strings.HasSuffix(entry.Name(),".json"){return fmt.Errorf("unexpected legacy %s file %q",folder,entry.Name())}
        name:=strings.TrimSuffix(entry.Name(),".json")
        path:=filepath.Join(dir,folder,entry.Name())
        blob,err:=os.ReadFile(path)
        if err!=nil{return err}
        if err=consume(name,blob);err!=nil{return fmt.Errorf("%s: %w",path,err)}
    }
    return nil
}

func scanLegacyJSONL(path string,consume func([]byte)error)error {
    file,err:=os.Open(path)
    if errors.Is(err,os.ErrNotExist){return nil}
    if err!=nil{return err}
    defer file.Close()
    reader:=bufio.NewScanner(file)
    reader.Buffer(make([]byte,4096),8<<20)
    line:=0
    for reader.Scan() {
        line++
        blob:=reader.Bytes()
        if len(blob)==0 {continue}
        if err:=consume(blob);err!=nil {
            return fmt.Errorf("%s line %d: %w",path,line,err)
        }
    }
    return reader.Err()
}
