package storage

import (
    "context"
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "crypto/sha256"
    "database/sql"
    "encoding/base64"
    "encoding/hex"
    "encoding/json"
    "errors"
    "io"
    "os"
    "sort"
    "strings"
    "time"
)

// Only explicitly provisioned refresh credentials can be sealed. This is an
// internal storage mechanism, NOT an OAuth grant issuer or a source of
// autonomous authorization. An external authorization server and verified
// refresh flow are required before a credential can safely drive effects.
var ErrGrantUnavailable = errors.New("delegated credential unavailable or unauthorized")
var ErrGrantKeyUnavailable = errors.New("delegated credential encryption key unavailable")
var ErrGrantConflict = errors.New("delegated credential state changed")
var ErrGrantRefreshPending = errors.New("delegated credential refresh outcome requires reauthorization")

const maxRefreshCredential = 8192
const maxGrantLifetime = 30 * 24 * time.Hour
const grantKeyringEnv = "REGATE_GRANT_KEYS_JSON"

// Keyring JSON is supplied only through the process environment:
// {"active":"k2","keys":{"k1":"BASE64_32_BYTES","k2":"BASE64_32_BYTES"}}
// The key material is never written to SQLite, config, traces or API output.
type GrantKeyRing struct {
    active string
    keys map[string][]byte
}

func validGrantKeyID(id string) bool {
    if len(id)==0 || len(id)>40{return false}
    for _,r:=range id {
        if !(r>='a'&&r<='z' || r>='A'&&r<='Z' ||
            r>='0'&&r<='9' || r=='_' || r=='-') {return false}
    }
    return true
}
func LoadGrantKeyRing()(*GrantKeyRing,error) {
    raw:=os.Getenv(grantKeyringEnv)
    if raw=="" {return nil,nil}
    if len(raw)>4096 {return nil,ErrGrantKeyUnavailable}
    var encoded struct {
        Active string `json:"active"`
        Keys map[string]string `json:"keys"`
    }
    if err:=json.Unmarshal([]byte(raw),&encoded);err!=nil{return nil,ErrGrantKeyUnavailable}
    if len(encoded.Keys)==0 || len(encoded.Keys)>8 || !validGrantKeyID(encoded.Active) {
        return nil,ErrGrantKeyUnavailable
    }
    keyring:=&GrantKeyRing{active:encoded.Active,keys:make(map[string][]byte,len(encoded.Keys))}
    for id,encodedKey:=range encoded.Keys {
        if !validGrantKeyID(id) {return nil,ErrGrantKeyUnavailable}
        secret,err:=base64.StdEncoding.Strict().DecodeString(encodedKey)
        if err!=nil || len(secret)!=32 {return nil,ErrGrantKeyUnavailable}
        keyring.keys[id]=secret
    }
    if _,ok:=keyring.keys[keyring.active];!ok{return nil,ErrGrantKeyUnavailable}
    return keyring,nil
}

type GrantMetadata struct {
    TaskID string `json:"task_id"`
    OwnerDigest string `json:"-"`
    Revision string `json:"revision"`
    Provider string `json:"provider"`
    Issuer string `json:"issuer"`
    Audience string `json:"audience"`
    ScopesDigest string `json:"scopes_digest"`
    Generation int64 `json:"generation"`
    ExpiresAt time.Time `json:"expires_at"`
    State string `json:"state"`
    KeyID string `json:"key_id"`
}
type taskGrant struct {
    GrantMetadata
    nonce []byte
    ciphertext []byte
}

func digestTaskScopes(scopes []string)string {
    ordered:=append([]string(nil),scopes...)
    sort.Strings(ordered)
    raw,_:=json.Marshal(ordered)
    digest:=sha256.Sum256(raw)
    return hex.EncodeToString(digest[:])
}
func validRefreshCredential(value string)bool {
    return len(value)>0 && len(value)<=maxRefreshCredential &&
        strings.TrimSpace(value)==value && !strings.ContainsAny(value,"\x00\r\n\t ")
}
func validGrantExpiration(expiration time.Time)bool {
    life:=time.Until(expiration)
    return life>30*time.Second && life<=maxGrantLifetime
}

func grantAAD(metadata GrantMetadata)[]byte {
    // The struct preserves a stable field order; include every plain-text
    // authorization field in authenticated associated data.
    raw,_:=json.Marshal(struct {
        Domain string `json:"domain"`
        Task string `json:"task"`
        Owner string `json:"owner"`
        Revision string `json:"revision"`
        Provider string `json:"provider"`
        Issuer string `json:"issuer"`
        Audience string `json:"audience"`
        Scopes string `json:"scopes"`
        Generation int64 `json:"generation"`
        Expiration int64 `json:"expiration"`
        Key string `json:"key"`
    }{
        Domain:"regate/task-grant/aes-gcm/v1",
        Task:metadata.TaskID,Owner:metadata.OwnerDigest,
        Revision:metadata.Revision,Provider:metadata.Provider,
        Issuer:metadata.Issuer,Audience:metadata.Audience,
        Scopes:metadata.ScopesDigest,Generation:metadata.Generation,
        Expiration:metadata.ExpiresAt.UnixNano(),Key:metadata.KeyID,
    })
    return raw
}
func (k *GrantKeyRing) seal(metadata GrantMetadata,secret string)([]byte,[]byte,error) {
    if k==nil{return nil,nil,ErrGrantKeyUnavailable}
    key,ok:=k.keys[metadata.KeyID]
    if !ok{return nil,nil,ErrGrantKeyUnavailable}
    block,err:=aes.NewCipher(key)
    if err!=nil{return nil,nil,ErrGrantKeyUnavailable}
    gcm,err:=cipher.NewGCM(block)
    if err!=nil{return nil,nil,ErrGrantKeyUnavailable}
    nonce:=make([]byte,gcm.NonceSize())
    if _,err=io.ReadFull(rand.Reader,nonce);err!=nil{return nil,nil,err}
    return nonce,gcm.Seal(nil,nonce,[]byte(secret),grantAAD(metadata)),nil
}
func (k *GrantKeyRing) open(metadata GrantMetadata,nonce,ciphertext []byte)(string,error) {
    if k==nil{return "",ErrGrantKeyUnavailable}
    key,ok:=k.keys[metadata.KeyID]
    if !ok{return "",ErrGrantKeyUnavailable}
    block,err:=aes.NewCipher(key)
    if err!=nil{return "",ErrGrantKeyUnavailable}
    gcm,err:=cipher.NewGCM(block)
    if err!=nil{return "",ErrGrantKeyUnavailable}
    if len(nonce)!=gcm.NonceSize() || len(ciphertext)<gcm.Overhead() {
        return "",ErrGrantUnavailable
    }
    data,err:=gcm.Open(nil,nonce,ciphertext,grantAAD(metadata))
    if err!=nil{return "",ErrGrantUnavailable}
    token:=string(data)
    for i:=range data {data[i]=0}
    if !validRefreshCredential(token){return "",ErrGrantUnavailable}
    return token,nil
}

func (s *Store) grantRowTx(tx *sql.Tx,taskID string)(taskGrant,error) {
    var row taskGrant
    var expires int64
    err:=tx.QueryRow(`SELECT task_id,owner_digest,revision,provider,issuer,audience,
        scopes_digest,generation,expires_ns,state,key_id,nonce,ciphertext
        FROM task_grants WHERE task_id=?`,taskID).Scan(
        &row.TaskID,&row.OwnerDigest,&row.Revision,&row.Provider,
        &row.Issuer,&row.Audience,&row.ScopesDigest,&row.Generation,
        &expires,&row.State,&row.KeyID,&row.nonce,&row.ciphertext)
    if err!=nil{return taskGrant{},err}
    row.ExpiresAt=time.Unix(0,expires).UTC()
    return row,nil
}
func grantTaskTx(tx *sql.Tx,taskID,owner string)(TaskRecord,error) {
    var raw,status string
    err:=tx.QueryRow("SELECT record,status FROM tasks WHERE task_id=?",taskID).
        Scan(&raw,&status)
    if err!=nil{return TaskRecord{},err}
    var task TaskRecord
    if err=json.Unmarshal([]byte(raw),&task);err!=nil{return TaskRecord{},err}
    if task.TaskID!=taskID || task.OwnerDigest!=owner ||
        task.Delegation==nil || len(task.RequiredScopes)==0 ||
        (task.Status!="working" && task.Status!="input_required") ||
        task.Status!=status {
        return TaskRecord{},ErrGrantUnavailable
    }
    if task.TTLMS>0 && time.Since(task.CreatedAt)>time.Duration(task.TTLMS)*time.Millisecond {
        return TaskRecord{},ErrGrantUnavailable
    }
    return task,nil
}

// ReserveTaskGrantProvision durably records ONE initial refresh-token exchange
// before it contacts the OAuth provider. A failed or lost response never lets
// a second caller use the same task as a retry lane. Provisioning is restricted
// to an explicitly suspended delegated task and cannot widen its original
// issuer, audience or permission scopes.
func (s *Store) ReserveTaskGrantProvision(ctx context.Context,
    taskID,owner string)(GrantMetadata,error) {
    if s.grantKeys==nil{return GrantMetadata{},ErrGrantKeyUnavailable}
    if len(taskID)!=24 || len(owner)!=64{return GrantMetadata{},ErrGrantUnavailable}
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return GrantMetadata{},err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return GrantMetadata{},err}
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil || task.Status!="input_required"{
        return GrantMetadata{},ErrGrantUnavailable
    }
    meta:=GrantMetadata{
        TaskID:taskID,OwnerDigest:owner,Revision:task.Revision,
        Provider:task.Delegation.Provider,Issuer:task.Delegation.Issuer,
        Audience:task.Delegation.Audience,
        ScopesDigest:digestTaskScopes(task.RequiredScopes),
        Generation:1,ExpiresAt:time.Now().Add(29*24*time.Hour).UTC(),
        State:"refreshing",KeyID:s.grantKeys.active,
    }
    if meta.Provider=="" || meta.Issuer=="" || meta.Audience==""{
        return GrantMetadata{},ErrGrantUnavailable
    }
    _,err=tx.Exec(`INSERT INTO task_grants(task_id,owner_digest,revision,provider,
        issuer,audience,scopes_digest,generation,expires_ns,state,key_id,
        nonce,ciphertext,updated_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
        taskID,owner,meta.Revision,meta.Provider,meta.Issuer,meta.Audience,
        meta.ScopesDigest,meta.Generation,meta.ExpiresAt.UnixNano(),meta.State,
        meta.KeyID,[]byte{},[]byte{},time.Now().UnixNano())
    if err!=nil {
        if strings.Contains(err.Error(),"constraint"){return GrantMetadata{},ErrGrantConflict}
        return GrantMetadata{},err
    }
    if err=tx.Commit();err!=nil{return GrantMetadata{},err}
    return meta,nil
}

// CreateTaskGrant seals an explicitly provisioned credential for ONE existing,
// delegated task. Initial credential validation is the job of an OAuth-aware
// caller; this storage method never claims that an arbitrary string is a valid
// refresh token. No automatic retrieval from an inbound access token.
func (s *Store) CreateTaskGrant(ctx context.Context,taskID,owner,credential string,
    expiration time.Time)(GrantMetadata,error) {
    if s.grantKeys==nil{return GrantMetadata{},ErrGrantKeyUnavailable}
    if !validRefreshCredential(credential) || !validGrantExpiration(expiration) ||
        len(taskID)!=24 || len(owner)!=64 {
        return GrantMetadata{},ErrGrantUnavailable
    }
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return GrantMetadata{},err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return GrantMetadata{},err}
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil{return GrantMetadata{},ErrGrantUnavailable}
    metadata:=GrantMetadata{
        TaskID:taskID,OwnerDigest:owner,Revision:task.Revision,
        Provider:task.Delegation.Provider,Issuer:task.Delegation.Issuer,
        Audience:task.Delegation.Audience,
        ScopesDigest:digestTaskScopes(task.RequiredScopes),
        Generation:1,ExpiresAt:expiration.UTC(),State:"active",
        KeyID:s.grantKeys.active,
    }
    if metadata.Provider=="" || metadata.Issuer=="" || metadata.Audience=="" {
        return GrantMetadata{},ErrGrantUnavailable
    }
    nonce,sealed,err:=s.grantKeys.seal(metadata,credential)
    if err!=nil{return GrantMetadata{},err}
    _,err=tx.Exec(`INSERT INTO task_grants(task_id,owner_digest,revision,provider,
        issuer,audience,scopes_digest,generation,expires_ns,state,key_id,
        nonce,ciphertext,updated_ns) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
        taskID,owner,metadata.Revision,metadata.Provider,metadata.Issuer,
        metadata.Audience,metadata.ScopesDigest,metadata.Generation,
        metadata.ExpiresAt.UnixNano(),metadata.State,metadata.KeyID,nonce,
        sealed,time.Now().UnixNano())
    if err!=nil {
        if strings.Contains(err.Error(),"constraint") {return GrantMetadata{},ErrGrantConflict}
        return GrantMetadata{},err
    }
    if err=tx.Commit();err!=nil{return GrantMetadata{},err}
    return metadata,nil
}
func validateGrantOwner(row taskGrant,task TaskRecord,owner string)error {
    if row.OwnerDigest!=owner || row.TaskID!=task.TaskID ||
        row.Revision!=task.Revision || task.Delegation==nil ||
        row.Provider!=task.Delegation.Provider ||
        row.Issuer!=task.Delegation.Issuer ||
        row.Audience!=task.Delegation.Audience ||
        row.ScopesDigest!=digestTaskScopes(task.RequiredScopes) {
        return ErrGrantUnavailable
    }
    if !row.ExpiresAt.After(time.Now()) {
        return ErrGrantUnavailable
    }
    return nil
}

// TaskGrantState reports non-secret metadata. No ciphertext or credential is
// ever included in its return value.
func (s *Store) TaskGrantState(ctx context.Context,taskID,owner string)(GrantMetadata,error) {
    tx,err:=s.db.BeginTx(ctx,&sql.TxOptions{ReadOnly:true})
    if err!=nil{return GrantMetadata{},err}
    defer tx.Rollback()
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil{return GrantMetadata{},ErrGrantUnavailable}
    row,err:=s.grantRowTx(tx,taskID)
    if err!=nil{return GrantMetadata{},ErrGrantUnavailable}
    if err=validateGrantOwner(row,task,owner);err!=nil{return GrantMetadata{},err}
    return row.GrantMetadata,nil
}

// BeginTaskGrantRefresh is a one-shot durable barrier before contacting the
// OAuth refresh endpoint. An interrupted HTTP request MUST NOT be blindly
// repeated: the remote provider may have rotated its refresh token.
func (s *Store) BeginTaskGrantRefresh(ctx context.Context,taskID,owner string)(GrantMetadata,string,error) {
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return GrantMetadata{},"",err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return GrantMetadata{},"",err}
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil{return GrantMetadata{},"",ErrGrantUnavailable}
    row,err:=s.grantRowTx(tx,taskID)
    if err!=nil{return GrantMetadata{},"",ErrGrantUnavailable}
    if err=validateGrantOwner(row,task,owner);err!=nil{return GrantMetadata{},"",err}
    if row.State!="active"{return GrantMetadata{},"",ErrGrantRefreshPending}
    token,err:=s.grantKeys.open(row.GrantMetadata,row.nonce,row.ciphertext)
    if err!=nil{return GrantMetadata{},"",err}
    result,err:=tx.Exec(`UPDATE task_grants SET state='refreshing',updated_ns=?
        WHERE task_id=? AND generation=? AND state='active'`,
        time.Now().UnixNano(),taskID,row.Generation)
    if err!=nil{return GrantMetadata{},"",err}
    affected,err:=result.RowsAffected()
    if err!=nil || affected!=1{return GrantMetadata{},"",ErrGrantConflict}
    if err=tx.Commit();err!=nil{return GrantMetadata{},"",err}
    row.State="refreshing"
    return row.GrantMetadata,token,nil
}

// FinishTaskGrantRefresh must be called only after the caller verified the
// new issuer/audience/subject/scope and refresh response. Generation CAS
// prevents late actors from restoring a credential after revoke or rotation.
func (s *Store) FinishTaskGrantRefresh(ctx context.Context,meta GrantMetadata,
    newCredential string,expiration time.Time)(GrantMetadata,error) {
    if s.grantKeys==nil{return GrantMetadata{},ErrGrantKeyUnavailable}
    if !validRefreshCredential(newCredential) || !validGrantExpiration(expiration) {
        return GrantMetadata{},ErrGrantUnavailable
    }
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return GrantMetadata{},err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return GrantMetadata{},err}
    row,err:=s.grantRowTx(tx,meta.TaskID)
    if err!=nil{return GrantMetadata{},ErrGrantUnavailable}
    if row.OwnerDigest!=meta.OwnerDigest || row.Generation!=meta.Generation ||
        row.Revision!=meta.Revision || row.Provider!=meta.Provider ||
        row.Issuer!=meta.Issuer || row.Audience!=meta.Audience ||
        row.ScopesDigest!=meta.ScopesDigest || row.State!="refreshing" {
        return GrantMetadata{},ErrGrantConflict
    }
    next:=row.GrantMetadata
    next.Generation++
    next.KeyID=s.grantKeys.active
    next.State="active"
    next.ExpiresAt=expiration.UTC()
    nonce,sealed,err:=s.grantKeys.seal(next,newCredential)
    if err!=nil{return GrantMetadata{},err}
    affectedRow,err:=tx.Exec(`UPDATE task_grants SET state=?,generation=?,expires_ns=?,
        key_id=?,nonce=?,ciphertext=?,updated_ns=?
        WHERE task_id=? AND owner_digest=? AND generation=? AND state='refreshing'`,
        next.State,next.Generation,next.ExpiresAt.UnixNano(),next.KeyID,
        nonce,sealed,time.Now().UnixNano(),next.TaskID,next.OwnerDigest,meta.Generation)
    if err!=nil{return GrantMetadata{},err}
    affected,err:=affectedRow.RowsAffected()
    if err!=nil || affected!=1{return GrantMetadata{},ErrGrantConflict}
    if err=tx.Commit();err!=nil{return GrantMetadata{},err}
    return next,nil
}

// RevokeTaskGrant makes the credential unusable and removes its live ciphertext.
// SQLite backups and WAL may retain older encrypted pages, so key erasure and
// retention policies remain separate operator responsibilities.
func (s *Store) RevokeTaskGrant(ctx context.Context,taskID,owner string)error {
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return err}
    row,err:=s.grantRowTx(tx,taskID)
    if err!=nil{return ErrGrantUnavailable}
    if row.OwnerDigest!=owner{return ErrGrantUnavailable}
    if row.State=="revoked"{return ErrGrantUnavailable}
    res,err:=tx.Exec(`UPDATE task_grants SET state='revoked',generation=generation+1,
        nonce=X'',ciphertext=X'',updated_ns=? WHERE task_id=? AND owner_digest=? AND state!='revoked'`,
        time.Now().UnixNano(),taskID,owner)
    if err!=nil{return err}
    n,err:=res.RowsAffected()
    if err!=nil || n!=1{return ErrGrantConflict}
    return tx.Commit()
}

// RewrapTaskGrant migrates ciphertext to the current active key. Both keys
// must be configured and the grant must be quiescent (not refreshing).
func (s *Store) RewrapTaskGrant(ctx context.Context,taskID,owner string)(GrantMetadata,error) {
    if s.grantKeys==nil{return GrantMetadata{},ErrGrantKeyUnavailable}
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return GrantMetadata{},err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return GrantMetadata{},err}
    task,err:=grantTaskTx(tx,taskID,owner)
    if err!=nil{return GrantMetadata{},ErrGrantUnavailable}
    row,err:=s.grantRowTx(tx,taskID)
    if err!=nil{return GrantMetadata{},ErrGrantUnavailable}
    if err=validateGrantOwner(row,task,owner);err!=nil{return GrantMetadata{},err}
    if row.State!="active"{return GrantMetadata{},ErrGrantRefreshPending}
    token,err:=s.grantKeys.open(row.GrantMetadata,row.nonce,row.ciphertext)
    if err!=nil{return GrantMetadata{},err}
    next:=row.GrantMetadata
    next.Generation++
    next.KeyID=s.grantKeys.active
    nonce,sealed,err:=s.grantKeys.seal(next,token)
    if err!=nil{return GrantMetadata{},err}
    res,err:=tx.Exec(`UPDATE task_grants SET key_id=?,generation=?,nonce=?,
        ciphertext=?,updated_ns=? WHERE task_id=? AND owner_digest=?
        AND generation=? AND state='active'`,next.KeyID,next.Generation,nonce,
        sealed,time.Now().UnixNano(),taskID,owner,row.Generation)
    if err!=nil{return GrantMetadata{},err}
    n,err:=res.RowsAffected()
    if err!=nil || n!=1{return GrantMetadata{},ErrGrantConflict}
    if err=tx.Commit();err!=nil{return GrantMetadata{},err}
    return next,nil
}

// A crash or leadership takeover during remote refresh invalidates the
// credential rather than retrying an ambiguous token-rotation request.
func recoverRefreshingGrantsTx(tx *sql.Tx)error {
    _,err:=tx.Exec(`UPDATE task_grants SET state='needs_reauth',generation=generation+1,
        nonce=X'',ciphertext=X'',updated_ns=?
        WHERE state='refreshing'`,time.Now().UnixNano())
    return err
}

func (s *Store) MarkTaskGrantIndeterminate(ctx context.Context,meta GrantMetadata)error {
    tx,err:=s.db.BeginTx(ctx,nil)
    if err!=nil{return err}
    defer tx.Rollback()
    if err=s.checkExecutionLeaseTx(tx);err!=nil{return err}
    res,err:=tx.Exec(`UPDATE task_grants SET state='needs_reauth',generation=generation+1,
        nonce=X'',ciphertext=X'',updated_ns=?
        WHERE task_id=? AND owner_digest=? AND generation=? AND state='refreshing'`,
        time.Now().UnixNano(),meta.TaskID,meta.OwnerDigest,meta.Generation)
    if err!=nil{return err}
    n,err:=res.RowsAffected()
    if err!=nil || n!=1{return ErrGrantConflict}
    return tx.Commit()
}

// no shared endpoint exposes this method; it is only for trusted internal
// admission. A full unattended delegation flow requires independent OAuth
// verification and integration tests before being made public.
