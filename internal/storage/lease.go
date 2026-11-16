package storage

import (
    "context"
    "database/sql"
    "errors"
    "fmt"
    "time"
)

var ErrExecutionLeaseHeld = errors.New("another Regate worker holds the execution lease")
var ErrExecutionLeaseLost = errors.New("Regate execution lease lost; external effects are fenced")

const executionLeaseName = "default"
const leaseLifetime = 30 * time.Second
const leaseHeartbeat = 5 * time.Second

func leaseLiveTx(tx *sql.Tx,now int64)(bool,error) {
    var deadline int64
    err:=tx.QueryRow("SELECT deadline_ns FROM execution_leases WHERE name=?",executionLeaseName).Scan(&deadline)
    if errors.Is(err,sql.ErrNoRows){return false,nil}
    if err!=nil{return false,err}
    return deadline>now,nil
}
func (s *Store) liveExecutionLease()(bool,error) {
    var deadline int64
    err:=s.db.QueryRow("SELECT deadline_ns FROM execution_leases WHERE name=?",executionLeaseName).Scan(&deadline)
    if errors.Is(err,sql.ErrNoRows){return false,nil}
    if err!=nil{return false,err}
    return deadline>time.Now().UnixNano(),nil
}

// AcquireExecutionLease promotes a single process to execution leader.
// Monotone epochs prevent an old worker from renewing a lease after takeover.
// A new execution leader may only start after the existing lease is released
// or expires. The lease is internal to Regate; no upstream is trusted to
// enforce it unless an effect adapter explicitly supports fencing.
func (s *Store) AcquireExecutionLease()error {
    s.mu.RLock()
    already:=s.leaseOwner!=""
    s.mu.RUnlock()
    if already{return fmt.Errorf("execution lease already acquired by this store")}

    owner:=ID()
    tx,err:=s.db.BeginTx(context.Background(),nil)
    if err!=nil{return err}
    defer tx.Rollback()
    var prior string
    var epoch,deadline int64
    err=tx.QueryRow("SELECT holder,epoch,deadline_ns FROM execution_leases WHERE name=?",executionLeaseName).
        Scan(&prior,&epoch,&deadline)
    now:=time.Now().UnixNano()
    switch {
    case errors.Is(err,sql.ErrNoRows):
        epoch=1
        _,err=tx.Exec(`INSERT INTO execution_leases(name,holder,epoch,deadline_ns) VALUES(?,?,?,?)`,
            executionLeaseName,owner,epoch,now+int64(leaseLifetime))
    case err!=nil:
        return err
    case deadline>now:
        return ErrExecutionLeaseHeld
    default:
        epoch++
        _,err=tx.Exec(`UPDATE execution_leases SET holder=?,epoch=?,deadline_ns=?
            WHERE name=?`,owner,epoch,now+int64(leaseLifetime),executionLeaseName)
    }
    if err!=nil{return err}
    // Leader promotion and abandoned-work recovery share one transaction.
    // An observer might have opened the DB while the previous lease was
    // alive, then acquired leadership after it expired; recover at promotion,
    // not solely based on the earlier startup snapshot.
    if err=recoverDispatches(tx);err!=nil{return err}
    if err=recoverInterruptedTasksTx(tx);err!=nil{return err}
    if err=recoverRefreshingGrantsTx(tx);err!=nil{return err}
    if err=tx.Commit();err!=nil{return err}

    s.mu.Lock()
    s.leaseOwner=owner
    s.leaseEpoch=epoch
    s.leaseLost=false
    s.leaseStop=make(chan struct{})
    s.leaseDone=make(chan struct{})
    stop,done:=s.leaseStop,s.leaseDone
    s.mu.Unlock()
    go s.renewExecutionLease(owner,epoch,stop,done)
    return nil
}
func (s *Store) renewExecutionLease(owner string,epoch int64,stop <-chan struct{},done chan<- struct{}) {
    defer close(done)
    ticker:=time.NewTicker(leaseHeartbeat)
    defer ticker.Stop()
    for {
        select {
        case <-stop:
            return
        case <-ticker.C:
            now:=time.Now().UnixNano()
            result,err:=s.db.Exec(`UPDATE execution_leases SET deadline_ns=?
                WHERE name=? AND holder=? AND epoch=? AND deadline_ns>?`,
                now+int64(leaseLifetime),executionLeaseName,owner,epoch,now)
            if err!=nil {
                s.markLeaseLost(owner,epoch)
                return
            }
            affected,err:=result.RowsAffected()
            if err!=nil || affected!=1 {
                s.markLeaseLost(owner,epoch)
                return
            }
        }
    }
}
func (s *Store) markLeaseLost(owner string,epoch int64) {
    s.mu.Lock()
    if s.leaseOwner==owner && s.leaseEpoch==epoch {
        s.leaseLost=true
    }
    s.mu.Unlock()
}
func (s *Store) releaseExecutionLease() {
    s.mu.Lock()
    if s.leaseOwner=="" {
        s.mu.Unlock()
        return
    }
    owner,epoch:=s.leaseOwner,s.leaseEpoch
    stop,done:=s.leaseStop,s.leaseDone
    s.leaseLost=true
    s.leaseOwner=""
    s.mu.Unlock()

    close(stop)
    <-done
    _,_=s.db.Exec(`DELETE FROM execution_leases WHERE name=? AND holder=? AND epoch=?`,
        executionLeaseName,owner,epoch)
}

// CheckExecutionLease must be called at every potentially external effect
// boundary. Stores used only for offline import/admin and unit tests may not
// own a lease, but cannot dispatch while another live worker holds one.
func (s *Store) CheckExecutionLease(ctx context.Context)error {
    s.mu.RLock()
    owner,epoch,lost:=s.leaseOwner,s.leaseEpoch,s.leaseLost
    s.mu.RUnlock()
    if lost {return ErrExecutionLeaseLost}
    var actual string
    var actualEpoch,deadline int64
    err:=s.db.QueryRowContext(ctx,`SELECT holder,epoch,deadline_ns
        FROM execution_leases WHERE name=?`,executionLeaseName).
        Scan(&actual,&actualEpoch,&deadline)
    if errors.Is(err,sql.ErrNoRows) {
        if owner!="" {return ErrExecutionLeaseLost}
        return nil
    }
    if err!=nil{return err}
    if deadline<=time.Now().UnixNano(){return ErrExecutionLeaseLost}
    if owner=="" {return ErrExecutionLeaseHeld}
    if actual!=owner || actualEpoch!=epoch {return ErrExecutionLeaseLost}
    return nil
}

// RequireExecutionLeader is the mandatory engine admission rule. A Store
// opened for offline analysis cannot dispatch effects merely because no other
// worker happens to have acquired the lease yet.
func (s *Store) RequireExecutionLeader(ctx context.Context) error {
    s.mu.RLock()
    owner,lost:=s.leaseOwner,s.leaseLost
    s.mu.RUnlock()
    if lost{return ErrExecutionLeaseLost}
    if owner==""{return ErrExecutionLeaseHeld}
    return s.CheckExecutionLease(ctx)
}

// checkExecutionLeaseTx is the same barrier while an effect is being inserted
// transactionally. This prevents a dethroned leader from admitting new writes
// even if another Regate instance is concurrently promoting a new lease.
func (s *Store) checkExecutionLeaseTx(tx *sql.Tx)error {
    s.mu.RLock()
    owner,epoch,lost:=s.leaseOwner,s.leaseEpoch,s.leaseLost
    s.mu.RUnlock()
    if lost {return ErrExecutionLeaseLost}
    var actual string
    var actualEpoch,deadline int64
    err:=tx.QueryRow(`SELECT holder,epoch,deadline_ns
        FROM execution_leases WHERE name=?`,executionLeaseName).
        Scan(&actual,&actualEpoch,&deadline)
    if errors.Is(err,sql.ErrNoRows) {
        if owner!="" {return ErrExecutionLeaseLost}
        return nil
    }
    if err!=nil{return err}
    if deadline<=time.Now().UnixNano(){return ErrExecutionLeaseLost}
    if owner=="" {return ErrExecutionLeaseHeld}
    if owner!=actual || epoch!=actualEpoch {return ErrExecutionLeaseLost}
    return nil
}
