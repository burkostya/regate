package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/engine"
	"github.com/burkostya/regate/internal/mcp"
	"github.com/burkostya/regate/internal/storage"
)

const soakReadSurface = `
surface "soak_read" {
  path = "/mcp/soak-read"
  oauth = "acceptance"
  required_scopes = ["acceptance:access", "summary:read"]
  tool "read_seed" {
    source = "alpha.get_record"
  }
}
`

const soakSeed = "public-soak-seed"

// The child exercises the production HTTP handler and SQLite lease. A hard
// kill bypasses cleanup. The replacement waits for the real lease to expire.
func TestV1SoakGatewayProcess(t *testing.T) {
	sourcePath := os.Getenv("REGATE_SOAK_GATEWAY_SOURCE")
	if sourcePath == "" {
		return
	}
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(35 * time.Second)
	var gateway *Service
	for {
		gateway, err = New(os.Getenv("REGATE_SOAK_GATEWAY_STATE"), string(source))
		if !errors.Is(err, storage.ErrExecutionLeaseHeld) || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer gateway.Store.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Printf("READY http://%s/mcp\n", listener.Addr())
	server := &http.Server{Handler: gateway.Handler(), ReadHeaderTimeout: 8 * time.Second, IdleTimeout: 75 * time.Second}
	if err := server.Serve(listener); err != nil {
		t.Fatal(err)
	}
}

type soakProxy struct {
	*httptest.Server
	handler atomic.Pointer[httputil.ReverseProxy]
}

func newSoakProxy(t *testing.T) *soakProxy {
	t.Helper()
	p := &soakProxy{}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.handler.Load().ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *soakProxy) directTo(t *testing.T, child *standProcess, inspect func(*http.Response) error) {
	t.Helper()
	target, err := url.Parse(strings.TrimSuffix(child.url, "/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	p.handler.Store(&httputil.ReverseProxy{
		Rewrite:        func(r *httputil.ProxyRequest) { r.SetURL(target) },
		ModifyResponse: inspect,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "test upstream unavailable", http.StatusBadGateway)
		},
	})
}

type soakTraffic string

const (
	soakRead    soakTraffic = "read"
	soakPublish soakTraffic = "publish"
	soakLost    soakTraffic = "lost_reply"
	soakDeny    soakTraffic = "decision_deny"
	soakScope   soakTraffic = "missing_scope"
	soakScrub   soakTraffic = "scrubber_error"
	soakOffline soakTraffic = "beta_offline"
	soakCrash   soakTraffic = "gateway_crash"
	soakDown    soakTraffic = "gateway_offline"
)

type soakJob struct {
	kind  soakTraffic
	id    string
	token string
}

type soakResult struct {
	soakJob
	elapsed time.Duration
	err     error
}

type soakCommitBarrier struct {
	id        string
	committed chan struct{}
}

// Hold the upstream's successful reply until the gateway dies, so the crash
// happens after the persisted beta write and before Regate can confirm it.
func (b *soakCommitBarrier) hold(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK || resp.Request.Method != http.MethodPost {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	var reply struct {
		Result struct {
			StructuredContent struct {
				Record struct {
					ID string `json:"record_id"`
				}
			}
		}
	}
	if json.Unmarshal(raw, &reply) == nil && reply.Result.StructuredContent.Record.ID == b.id {
		close(b.committed)
		<-resp.Request.Context().Done()
		return resp.Request.Context().Err()
	}
	return nil
}

func runSoakJob(t *testing.T, client *http.Client, base string, alpha *mcp.Conn, job soakJob) soakResult {
	t.Helper()
	result := soakResult{soakJob: job}
	if job.kind == soakPublish || job.kind == soakLost || job.kind == soakScrub || job.kind == soakOffline || job.kind == soakCrash {
		notes := acceptanceNotes
		if job.kind == soakScrub {
			notes = acceptanceScrubFailure
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		seed, err := alpha.CallTool(ctx, "put_record", map[string]any{"record_id": job.id, "value": notes})
		cancel()
		if err != nil || seed["isError"] == true {
			result.err = errors.New("could not seed alpha through MCP")
			return result
		}
	}

	endpoint, tool := base+"/mcp/acceptance", "publish_summary"
	args := map[string]any{"record_id": job.id, "lose_reply": job.kind == soakLost}
	if job.kind == soakRead || job.kind == soakDown {
		endpoint, tool = base+"/mcp/soak-read", "read_seed"
		args = map[string]any{"record_id": soakSeed}
	}
	started := time.Now()
	result.err = checkSoakReply(client, latestRequest(t, endpoint, job.token, "tools/call", 1,
		map[string]any{"name": tool, "arguments": args}), job)
	result.elapsed = time.Since(started)
	return result
}

func checkSoakReply(client *http.Client, req *http.Request, job soakJob) error {
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("failed to receive HTTP response through the test proxy")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.New("reading HTTP response failed")
	}
	if bytes.Contains(raw, []byte(acceptanceEmail)) || bytes.Contains(raw, []byte(job.token)) {
		return errors.New("response exposed synthetic PII or bearer")
	}
	if job.kind == soakCrash || job.kind == soakDown {
		if resp.StatusCode != http.StatusBadGateway {
			return fmt.Errorf("crashed gateway: HTTP %d, want 502", resp.StatusCode)
		}
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d, want 200", resp.StatusCode)
	}
	var reply struct {
		Result *struct {
			IsError           bool
			StructuredContent map[string]any
		}
		Error *struct{ Code int }
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Result == nil || reply.Error != nil {
		return errors.New("expected a tool result over MCP")
	}
	switch job.kind {
	case soakRead:
		record, _ := reply.Result.StructuredContent["record"].(map[string]any)
		if reply.Result.IsError || record["record_id"] != soakSeed || record["value"] != "public sample" {
			return errors.New("unexpected read result")
		}
	case soakPublish:
		if reply.Result.IsError || reply.Result.StructuredContent["record_id"] != job.id {
			return errors.New("publish did not return the expected record ID")
		}
	default:
		if !reply.Result.IsError || reply.Result.StructuredContent != nil {
			return errors.New("refused or uncertain call returned successful or structured data")
		}
	}
	return nil
}

func TestV1Soak(t *testing.T) {
	rawDuration := os.Getenv("REGATE_SOAK_DURATION")
	if rawDuration == "" {
		t.Skip("run make test-v1-soak to set a sustained load profile")
	}
	duration, err := time.ParseDuration(rawDuration)
	if err != nil || duration < 9*time.Second || duration > 15*time.Minute {
		t.Fatal("REGATE_SOAK_DURATION must be between 9s and 15m")
	}
	const admin = "synthetic-soak-admin"
	t.Setenv("REGATE_ADMIN_TOKEN", admin)
	t.Setenv("REGATE_OTLP_TRACES_URL", "")
	root := t.TempDir()
	alpha := startStandProcess(t, "alpha", filepath.Join(root, "alpha"))
	beta := startStandProcess(t, "beta", filepath.Join(root, "beta"))
	alphaConn := mcp.NewConn(alpha.url, "", 5*time.Second)
	acceptanceProviderCall(t, alphaConn, "put_record", map[string]any{"record_id": soakSeed, "value": "public sample"})
	betaProxy := newSoakProxy(t)
	betaProxy.directTo(t, beta, nil)
	gatewayProxy := newSoakProxy(t)

	support, private, hooks := startAcceptanceSupport(t)
	template, err := os.ReadFile("../../examples/v1-acceptance.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer(
		"http://127.0.0.1:9201", strings.TrimSuffix(alpha.url, "/mcp"),
		"http://127.0.0.1:9202", betaProxy.URL,
		"http://127.0.0.1:9203", support.URL,
	).Replace(string(template)) + soakReadSurface
	cfg, err := config.Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	configPath, statePath := filepath.Join(root, "soak.hcl"), filepath.Join(root, "gateway")
	if err := os.WriteFile(configPath, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	startGateway := func() *standProcess {
		cmd := exec.Command(os.Args[0], "-test.run=^TestV1SoakGatewayProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "REGATE_SOAK_GATEWAY_SOURCE="+configPath, "REGATE_SOAK_GATEWAY_STATE="+statePath)
		return startStandChild(t, cmd, "gateway", statePath, 40*time.Second)
	}
	gateway := startGateway()
	gatewayProxy.directTo(t, gateway, nil)
	idleClient := &http.Client{Timeout: 20 * time.Second}

	const workers = 8
	var tokens [workers]string
	for i := range tokens {
		tokens[i] = signedTestJWTSubject(t, private, acceptanceIssuer, acceptanceAudience,
			"acceptance:access summary:read summary:write", fmt.Sprintf("soak-principal-%d", i))
	}
	reader := signedTestJWTSubject(t, private, acceptanceIssuer, acceptanceAudience,
		"acceptance:access summary:read", "soak-reader")
	sequence := 0
	makeJob := func(kind soakTraffic, worker int) soakJob {
		sequence++
		id := fmt.Sprintf("soak-%06d", sequence)
		token := tokens[worker]
		if kind == soakDeny {
			id = "DENY-" + id
		}
		if kind == soakScope {
			token = reader
		}
		return soakJob{kind: kind, id: id, token: token}
	}
	var results []soakResult
	record := func(result soakResult) {
		t.Helper()
		if result.err != nil {
			t.Fatalf("%s %s: %v", result.kind, result.id, result.err)
		}
		results = append(results, result)
	}
	execute := func(job soakJob) soakResult {
		return runSoakJob(t, idleClient, gatewayProxy.URL, alphaConn, job)
	}
	wave := func(jobs []soakJob) {
		replies := make([]soakResult, len(jobs))
		var wg sync.WaitGroup
		for i, job := range jobs {
			wg.Go(func() { replies[i] = execute(job) })
		}
		wg.Wait()
		for _, result := range replies {
			record(result)
		}
	}
	mix := []soakTraffic{soakRead, soakPublish, soakRead, soakPublish, soakDeny, soakRead,
		soakLost, soakPublish, soakScope, soakRead, soakScrub, soakPublish}
	phase := func(name string) {
		before := len(results)
		start := time.Now()
		deadline := start.Add(duration / 3)
		nextProgress := start.Add(30 * time.Second)
		for time.Now().Before(deadline) {
			roundStart := time.Now()
			jobs := make([]soakJob, workers)
			for worker := range jobs {
				jobs[worker] = makeJob(mix[sequence%len(mix)], worker)
			}
			wave(jobs)
			if time.Now().After(nextProgress) {
				t.Logf("phase=%s elapsed=%s calls=%d", name, time.Since(start).Round(time.Second), len(results)-before)
				nextProgress = nextProgress.Add(30 * time.Second)
			}
			if next := roundStart.Add(time.Second); next.Before(deadline) {
				time.Sleep(time.Until(next))
			} else {
				time.Sleep(time.Until(deadline))
			}
		}
		t.Logf("phase=%s elapsed=%s calls=%d", name, time.Since(start).Round(time.Millisecond), len(results)-before)
	}
	waitFor := func(signal <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-signal:
		case <-time.After(8 * time.Second):
			t.Fatalf("timed out waiting for %s", what)
		}
	}

	t.Logf("steady_duration=%s concurrent_clients=%d oauth_subjects=%d wave_interval=1s", duration, workers, workers+1)
	phase("before_faults")
	stopped := time.Now()
	beta.stop()
	// Mixed requests continue against the live gateway throughout the beta outage.
	for i := 0; i < 3; i++ {
		wave([]soakJob{makeJob(soakOffline, 0), makeJob(soakRead, 1), makeJob(soakDeny, 2), makeJob(soakScope, 3), makeJob(soakScrub, 4)})
		time.Sleep(time.Second)
	}
	beta = startStandProcess(t, "beta", beta.dir)
	betaProxy.directTo(t, beta, nil)
	t.Logf("beta_hard_kill_downtime=%s", time.Since(stopped).Round(time.Millisecond))
	phase("after_beta_restart")

	crash := makeJob(soakCrash, 0)
	barrier := &soakCommitBarrier{id: crash.id, committed: make(chan struct{})}
	betaProxy.directTo(t, beta, barrier.hold)
	crashedRequest := make(chan soakResult, 1)
	go func() { crashedRequest <- execute(crash) }()
	waitFor(barrier.committed, "beta's committed write before crashing gateway")
	wave([]soakJob{makeJob(soakRead, 1), makeJob(soakDeny, 2), makeJob(soakScrub, 3)})
	stopped = time.Now()
	gateway.stop()
	record(<-crashedRequest)
	betaProxy.directTo(t, beta, nil)
	wave([]soakJob{makeJob(soakDown, 1), makeJob(soakDown, 2), makeJob(soakDown, 3)})
	probe := makeJob(soakDown, 4)
	stopProbes := make(chan struct{})
	probed := make(chan []soakResult, 1)
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var replies []soakResult
		for {
			select {
			case <-ticker.C:
				replies = append(replies, execute(probe))
			case <-stopProbes:
				probed <- replies
				return
			}
		}
	}()
	defer func() {
		if stopProbes != nil {
			close(stopProbes)
			<-probed
		}
	}()
	gateway = startGateway()
	close(stopProbes)
	probes := <-probed
	stopProbes = nil
	for _, result := range probes {
		record(result)
	}
	gatewayProxy.directTo(t, gateway, nil)
	t.Logf("gateway_hard_kill_recovery=%s", time.Since(stopped).Round(time.Millisecond))
	phase("after_gateway_restart")
	checkSoakState(t, gatewayProxy.URL, admin, beta.url, cfg, results, append(tokens[:], reader), hooks)
}

func checkSoakState(t *testing.T, base, admin, betaURL string, cfg *config.Config, results []soakResult, tokens []string, hooks *acceptanceCalls) {
	t.Helper()
	beta := mcp.NewConn(betaURL, "", 5*time.Second)
	provider := func(tool string, args map[string]any) map[string]any {
		return acceptanceProviderCall(t, beta, tool, args)
	}
	var traces json.RawMessage
	acceptanceAdminGet(t, base+"/api/traces", admin, tokens[0], &traces)
	var rawEffects json.RawMessage
	acceptanceAdminGet(t, base+"/api/effects", admin, tokens[0], &rawEffects)
	for _, token := range tokens[1:] {
		checkAcceptancePrivacy(t, "retained traces", traces, token)
		checkAcceptancePrivacy(t, "effect journal", rawEffects, token)
	}
	var effects []storage.Effect
	if err := json.Unmarshal(rawEffects, &effects); err != nil {
		t.Fatal(err)
	}

	audits := make(map[int64]bool)
	checkAudit := func() {
		var entries []storage.AuditRecord
		acceptanceAdminGet(t, base+"/api/audit", admin, tokens[0], &entries)
		for _, entry := range entries {
			if entry.Action == "regate_effect_readback" && entry.Outcome == "succeeded" {
				audits[entry.Sequence] = true
			}
		}
	}

	byIntent := make(map[string]storage.Effect)
	for _, effect := range effects {
		if effect.Operation == "beta.put_record" {
			if effect.TargetDigest != storage.TargetDigest("mcp", cfg.Upstreams["beta"].URL) || byIntent[effect.IntentDigest].ID != "" {
				t.Fatal("unexpected beta target or duplicate journaled write")
			}
			byIntent[effect.IntentDigest] = effect
		}
	}
	byToken := make(map[string]storage.Effect, len(byIntent))
	for _, effect := range byIntent {
		token, err := engine.ReadbackOperationToken(effect.ID, cfg.Readbacks["beta_record"], cfg.Upstreams["beta"])
		if err != nil {
			t.Fatal(err)
		}
		byToken[token] = effect
	}

	counts := make(map[soakTraffic][]time.Duration)
	confirmed, resolved, committed := 0, 0, 0
	var unavailable []soakJob
	for _, result := range results {
		counts[result.kind] = append(counts[result.kind], result.elapsed)
		switch result.kind {
		case soakRead, soakDown:
			continue
		case soakOffline:
			unavailable = append(unavailable, result.soakJob)
		}
		stored := provider("get_record", map[string]any{"record_id": result.id})
		if result.kind != soakPublish && result.kind != soakLost && result.kind != soakCrash {
			if stored["found"] != false {
				t.Fatalf("%s created an unexpected beta record", result.kind)
			}
			continue
		}
		committed++
		value := "Summary for " + result.id + ": " + acceptanceRedacted
		assertRecord(t, stored, "beta", result.id, value)
		rec := stored["record"].(map[string]any)
		token, _ := rec["operation_token"].(string)
		fault := ""
		if result.kind == soakLost {
			fault = "drop_after_commit"
		}
		intent := map[string]any{"record_id": result.id, "value": value, "fault": fault}
		digest, err := storage.DigestIntent(map[string]any{"record_id": result.id, "value": value, "fault": fault, "operation_token": token})
		effect := byToken[token]
		if err != nil || effect.ID == "" || effect.IntentDigest != digest {
			t.Fatalf("%s is missing a matching token-bound effect", result.id)
		}
		delete(byIntent, digest)
		if result.kind == soakPublish {
			if effect.Status != "confirmed" {
				t.Fatalf("acknowledged record %s has journal status %s", result.id, effect.Status)
			}
			confirmed++
			continue
		}
		if effect.Status != "unknown" {
			t.Fatalf("uncertain record %s has journal status %s after restart", result.id, effect.Status)
		}
		reply := decode(t, jsonPost(t, base+"/api/effect-readback", admin, map[string]any{
			"effect_id": effect.ID, "adapter": "beta_record", "intent": intent,
		}))
		if reply["status"] != "resolved_applied" {
			t.Fatalf("verified readback did not resolve %s", result.id)
		}
		resolved++
		if resolved%50 == 0 {
			checkAudit()
		}
	}
	// Remaining write attempts were admitted while beta was unreachable.
	// Use the durable intent digest to match each to its absent remote record.
	for _, job := range unavailable {
		var matched storage.Effect
		value := "Summary for " + job.id + ": " + acceptanceRedacted
		for digest, effect := range byIntent {
			token, err := engine.ReadbackOperationToken(effect.ID, cfg.Readbacks["beta_record"], cfg.Upstreams["beta"])
			if err != nil {
				t.Fatal(err)
			}
			wire, err := storage.DigestIntent(map[string]any{"record_id": job.id, "value": value, "fault": "", "operation_token": token})
			if err != nil {
				t.Fatal(err)
			}
			if wire == digest && effect.Status == "unknown" {
				delete(byIntent, digest)
				matched = effect
				break
			}
		}
		if matched.ID == "" {
			t.Fatalf("unavailable write %s is missing an unknown journal entry", job.id)
		}
		response := jsonPost(t, base+"/api/effect-readback", admin, map[string]any{
			"effect_id": matched.ID, "adapter": "beta_record",
			"intent": map[string]any{"record_id": job.id, "value": value, "fault": ""},
		})
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("readback without a committed record returned HTTP %d", response.StatusCode)
		}
	}
	if len(byIntent) != 0 || confirmed == 0 || resolved != len(counts[soakLost])+1 {
		t.Fatal("unexpected write outcome totals or unaccounted journal entries")
	}
	stats := provider("stats", nil)
	assertStandCount(t, stats, "writes_received", float64(committed))
	assertStandCount(t, stats, "writes_committed", float64(committed))
	assertStandCount(t, stats, "idempotent_replays", 0)
	if hooks.agent.Load() != int64(committed+len(unavailable)) ||
		hooks.scrubber.Load() != int64(committed+len(unavailable)+len(counts[soakScrub])) {
		t.Fatal("unexpected dispatch count at the agent or scrubber boundary")
	}
	checkAudit()
	if len(audits) != resolved {
		t.Fatalf("audited resolutions=%d, want %d", len(audits), resolved)
	}
	for _, kind := range []soakTraffic{soakRead, soakPublish, soakLost, soakDeny, soakScope, soakScrub, soakOffline, soakCrash, soakDown} {
		samples := counts[kind]
		if len(samples) == 0 {
			t.Fatalf("missing traffic class %s", kind)
		}
		slices.Sort(samples)
		t.Logf("traffic=%s count=%d p95=%s max=%s", kind, len(samples), samples[(95*len(samples)+99)/100-1].Round(time.Millisecond), samples[len(samples)-1].Round(time.Millisecond))
	}
	t.Logf("beta_write_attempts=%d beta_commits=%d confirmed=%d resolved_applied=%d verified_readback_audits=%d unknown_unavailable=%d",
		committed, committed, confirmed, resolved, len(audits), len(unavailable))
}
