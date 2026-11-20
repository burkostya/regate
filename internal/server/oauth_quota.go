package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/burkostya/regate/internal/identity"
	"github.com/burkostya/regate/internal/storage"
)

type oauthQuota struct {
	key      []byte
	interval time.Duration
	burst    int
}

func oauthQuotaFromEnv() (*oauthQuota, error) {
	const keyName = "REGATE_OAUTH_QUOTA_KEY"
	const rateName = "REGATE_OAUTH_QUOTA_RPM"
	const burstName = "REGATE_OAUTH_QUOTA_BURST"
	raw := os.Getenv(keyName)
	if raw == "" {
		if os.Getenv(rateName) != "" || os.Getenv(burstName) != "" {
			return nil, errors.New("REGATE_OAUTH_QUOTA_KEY is required when configuring OAuth quotas")
		}
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != sha256.Size {
		return nil, errors.New("REGATE_OAUTH_QUOTA_KEY must contain 32 hex-encoded bytes")
	}
	rate, err := boundedPositiveEnv(rateName, 600)
	if err != nil {
		return nil, err
	}
	burst, err := boundedPositiveEnv(burstName, 60)
	if err != nil {
		return nil, err
	}
	interval := (time.Minute + time.Duration(rate-1)) / time.Duration(rate)
	return &oauthQuota{key: key, interval: interval, burst: burst}, nil
}

func (q *oauthQuota) allow(store *storage.Store, w http.ResponseWriter, r *http.Request, p identity.Principal) bool {
	if q == nil {
		return true
	}
	digest := hmac.New(sha256.New, q.key)
	fmt.Fprintf(digest, "%d:%s%s", len(p.Issuer), p.Issuer, p.Subject)
	var principal [32]byte
	digest.Sum(principal[:0])
	retry, err := store.AdmitOAuthRequest(r.Context(), principal, q.interval, q.burst)
	if err != nil {
		http.Error(w, "OAuth quota unavailable", http.StatusServiceUnavailable)
		return false
	}
	if retry > 0 {
		seconds := (retry + time.Second - 1) / time.Second
		w.Header().Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
		http.Error(w, "OAuth request quota exceeded", http.StatusTooManyRequests)
		return false
	}
	return true
}
