package testmcp

import (
    "strings"
    "sync"
    "testing"
)

// Concurrent requests arriving at one provider must linearize on the same
// persisted operation token. There is one original record and one inverse,
// regardless of scheduling; fsynced state survives a fresh server instance.
func TestDurableNativeIdempotenceAndCompensationUnderContention(t *testing.T) {
    dir:=t.TempDir()
    s,err:=New("alpha",dir)
    if err!=nil {t.Fatal(err)}
    if err:=s.advanceFence(7);err!=nil {t.Fatal(err)}
    token:=strings.Repeat("a",64)
    const goroutines=12
    type result struct {replay bool;err error}
    results:=make(chan result,goroutines)
    var wg sync.WaitGroup
    for i:=0;i<goroutines;i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            replay,err:=s.put("shared","immutable",token,7)
            results<-result{replay,err}
        }()
    }
    wg.Wait()
    close(results)
    initial,replays:=0,0
    for r:=range results {
        if r.err!=nil {t.Fatalf("same-key concurrent replay rejected: %v",r.err)}
        if r.replay {replays++} else {initial++}
    }
    if initial!=1 || replays!=goroutines-1 {
        t.Fatalf("concurrent native idempotence failed: initial=%d replay=%d",initial,replays)
    }
    stats:=s.stats()
    if stats["writes_committed"]!=int64(1) ||
        stats["idempotent_replays"]!=int64(goroutines-1) {
        t.Fatalf("write counters incorrect: %#v",stats)
    }
    if replay,err:=s.put("shared","changed",token,7);err==nil || replay {
        t.Fatalf("same token allowed payload modification: replay=%t err=%v",replay,err)
    }
    if replay,err:=s.put("other","immutable",token,7);err==nil || replay {
        t.Fatalf("same token reused for another record: replay=%t err=%v",replay,err)
    }
    inverse:=make(chan error,goroutines)
    for i:=0;i<goroutines;i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            inverse<-s.revoke("shared",7)
        }()
    }
    wg.Wait()
    close(inverse)
    applied,rejected:=0,0
    for err:=range inverse {
        if err==nil {applied++} else {rejected++}
    }
    if applied!=1 || rejected!=goroutines-1 {
        t.Fatalf("concurrent inverse was not unique: applied=%d rejected=%d",applied,rejected)
    }
    if s.stats()["compensations_committed"]!=int64(1) {
        t.Fatal("provider committed duplicate compensation")
    }
    if replay,err:=s.put("shared","immutable",token,7);err==nil || replay {
        t.Fatalf("tombstoned record revived by same token: replay=%t err=%v",replay,err)
    }
    if err:=s.advanceFence(8);err!=nil {t.Fatal(err)}
    if replay,err:=s.put("future","fresh",strings.Repeat("b",64),7);
        err==nil || replay {
        t.Fatalf("old worker bypassed provider epoch: replay=%t err=%v",replay,err)
    }
    if err:=s.revoke("shared",7);err==nil {
        t.Fatal("stale worker compensated after epoch takeover")
    }

    fresh,err:=New("alpha",dir)
    if err!=nil {t.Fatalf("fsynced provider state invalid on restart: %v",err)}
    _,storedToken,compensated,present:=fresh.lookup("shared")
    if !present || !compensated || storedToken!=token {
        t.Fatalf("restart lost tombstone and original token: %v %v %q",present,compensated,storedToken)
    }
    recovered:=fresh.stats()
    if recovered["fence_epoch"]!=uint64(8) ||
        recovered["writes_committed"]!=int64(1) ||
        recovered["idempotent_replays"]!=int64(goroutines-1) ||
        recovered["compensations_committed"]!=int64(1) {
        t.Fatalf("restart lost native provider safety history: %#v",recovered)
    }
}
