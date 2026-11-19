package server

import (
    "crypto/rand"
    "crypto/sha256"
    "errors"
    "fmt"
    "net"
    "net/netip"
    "os"
    "strconv"
    "strings"
    "sync"
    "time"
)

// Peer quotas are deliberately based on the socket peer, NEVER on spoofable
// X-Forwarded-For / Forwarded headers. Operators behind a reverse proxy must
// enforce authenticated end-user limits at that trusted proxy as well.
const maxRatePeers = 1024

type peerBucket struct {
    tokens float64
    last time.Time
}
type peerRateLimiter struct {
    mu sync.Mutex
    secret [32]byte
    rate float64
    burst float64
    peers map[[32]byte]peerBucket
    now func()time.Time
}

func newPeerRateLimiter(rps,burst int)(*peerRateLimiter,error) {
    if rps<1 || burst<1 || rps>10000 || burst>10000 {
        return nil,errors.New("per-peer requests and burst must be 1..10000")
    }
    limiter:=&peerRateLimiter{
        rate:float64(rps),burst:float64(burst),
        now:time.Now,peers:make(map[[32]byte]peerBucket),
    }
    if _,err:=rand.Read(limiter.secret[:]);err!=nil {return nil,fmt.Errorf("randomize peer quota keys: %w",err)}
    return limiter,nil
}

func boundedPositiveEnv(name string,defaultValue int)(int,error) {
    raw:=strings.TrimSpace(os.Getenv(name))
    if raw=="" {return defaultValue,nil}
    parsed,err:=strconv.Atoi(raw)
    if err!=nil || parsed<1 || parsed>10000 {
        return 0,fmt.Errorf("%s must be an integer in 1..10000",name)
    }
    return parsed,nil
}

func rateLimiterFromEnv(prefix string,rps,burst int)(*peerRateLimiter,error) {
    rate,err:=boundedPositiveEnv("REGATE_"+prefix+"_RPS",rps)
    if err!=nil{return nil,err}
    capacity,err:=boundedPositiveEnv("REGATE_"+prefix+"_BURST",burst)
    if err!=nil{return nil,err}
    return newPeerRateLimiter(rate,capacity)
}

func (l *peerRateLimiter) allow(remoteAddr string) bool {
    if l==nil {return true}
    host,_,err:=net.SplitHostPort(remoteAddr)
    if err!=nil {return false}
    addr,err:=netip.ParseAddr(host)
    if err!=nil {return false}
    addr=addr.Unmap().WithZone("")
    // A secret per-process hash eliminates retained raw IP addresses while
    // ensuring new ephemeral TCP ports cannot bypass a client's budget.
    h:=sha256.New()
    h.Write(l.secret[:])
    h.Write([]byte(addr.String()))
    var key [32]byte
    copy(key[:],h.Sum(nil))
    now:=l.now()
    l.mu.Lock()
    defer l.mu.Unlock()
    bucket,found:=l.peers[key]
    if !found {
        if len(l.peers)>=maxRatePeers {
            for k,v:=range l.peers {
                if now.Sub(v.last)>5*time.Minute {delete(l.peers,k)}
            }
            // Never allocate unbounded metadata for rotating source IPs.
            if len(l.peers)>=maxRatePeers {return false}
        }
        bucket=peerBucket{tokens:l.burst,last:now}
    } else if now.After(bucket.last) {
        bucket.tokens+=now.Sub(bucket.last).Seconds()*l.rate
        if bucket.tokens>l.burst {bucket.tokens=l.burst}
    }
    bucket.last=now
    if bucket.tokens<1 {
        l.peers[key]=bucket
        return false
    }
    bucket.tokens--
    l.peers[key]=bucket
    return true
}
