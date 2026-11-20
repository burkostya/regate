package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/storage"
)

func TestOAuthQuotaSurvivesGatewayAndProxyChanges(t *testing.T) {
	t.Setenv("REGATE_OAUTH_QUOTA_KEY", strings.Repeat("01", 32))
	t.Setenv("REGATE_OAUTH_QUOTA_RPM", "1")
	t.Setenv("REGATE_OAUTH_QUOTA_BURST", "2")
	primary, backend, world, private := oauthFixture(t)
	_, source, err := primary.Store.Active()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(primary.Store.Dir, source); !errors.Is(err, storage.ErrExecutionLeaseHeld) {
		t.Fatalf("concurrent leader was not excluded: %v", err)
	}

	first, second := newSoakProxy(t), newSoakProxy(t)
	for _, proxy := range []*soakProxy{first, second} {
		proxy.directTo(t, &standProcess{url: backend.URL + "/mcp"}, nil)
	}
	token := func(subject, scopes string) string {
		return signedTestJWTSubject(t, private, "https://issuer.test", "https://regate.test/mcp/support", scopes, subject)
	}
	primaryToken := token("principal-a", "surface:read crm:read")
	reissued := token("principal-a", "crm:read surface:read")
	other := token("principal-b", "surface:read crm:read")
	client := &http.Client{Timeout: 8 * time.Second}
	call := func(base, bearer string, want int) {
		t.Helper()
		req := latestRequest(t, base+"/mcp/support", bearer, "tools/call", 1,
			map[string]any{"name": "get_customer_direct", "arguments": map[string]any{"customer_id": "C1"}})
		req.Header.Set("X-Forwarded-For", "192.0.2.17")
		req.Header.Set("Forwarded", "for=192.0.2.18")
		resp, err := client.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		if got := resp.StatusCode; got != want {
			t.Errorf("quota HTTP %d, want %d", got, want)
		}
		if want == http.StatusTooManyRequests && resp.Header.Get("Retry-After") == "" {
			t.Error("quota refusal lacks Retry-After")
		}
	}
	var requests sync.WaitGroup
	requests.Go(func() { call(first.URL, primaryToken, http.StatusOK) })
	requests.Go(func() { call(second.URL, reissued, http.StatusOK) })
	requests.Wait()
	call(second.URL, primaryToken, http.StatusTooManyRequests)
	call(backend.URL, reissued, http.StatusTooManyRequests)

	backend.Close()
	if err := primary.Store.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := New(primary.Store.Dir, source)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Store.Close()
	replacement := httptest.NewServer(next.Handler())
	defer replacement.Close()
	second.directTo(t, &standProcess{url: replacement.URL + "/mcp"}, nil)
	call(second.URL, reissued, http.StatusTooManyRequests)
	call(second.URL, other, http.StatusOK)
	var metrics map[string]any
	acceptanceAdminGet(t, replacement.URL+"/api/metrics", "test-secret", reissued, &metrics)
	world.mu.Lock()
	defer world.mu.Unlock()
	calls := 0
	for _, method := range world.calls {
		if method == "tools/call" {
			calls++
		}
	}
	if calls != 3 {
		t.Fatalf("three admitted calls should reach the backend, got %d", calls)
	}
}
