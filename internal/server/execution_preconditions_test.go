package server

import (
    "context"
    "strings"
    "testing"
)

func TestExecutionPreconditionsFailClosedBeforeExternalEffects(t *testing.T) {
    svc, server, world := fixture(t)
    _, active := svc.Current()
    draft, err := svc.Store.DraftCreate()
    if err != nil { t.Fatal(err) }
    arguments := map[string]any{
        "surface": "support", "tool": "summarize_customer",
        "arguments": map[string]any{"customer_id":"C123"},
    }
    request := func(extra map[string]any) map[string]any {
        value := map[string]any{}
        for k, v := range arguments {value[k] = v}
        for k, v := range extra {value[k] = v}
        return value
    }
    world.mu.Lock()
    initialCalls := len(world.calls)
    world.mu.Unlock()

    cases := []struct {
        endpoint string
        description string
        extra map[string]any
    }{
        {"run", "unactivated draft", map[string]any{"draft_id":draft.ID}},
        {"run", "even empty draft ID", map[string]any{"draft_id":""}},
        {"run", "stale active revision", map[string]any{"expected_revision":"stale"}},
        {"run", "wrong revision type", map[string]any{"expected_revision":123}},
        {"run", "digest without draft", map[string]any{"expected_source_digest":draft.SourceDigest}},
        {"test", "digest without draft", map[string]any{"expected_source_digest":draft.SourceDigest}},
        {"test", "unreviewed stale draft", map[string]any{"draft_id":draft.ID,"expected_source_digest":strings.Repeat("0",64)}},
        {"test", "mixed source preconditions", map[string]any{"draft_id":draft.ID,"expected_revision":active.ID}},
        {"simulate", "empty draft identifier", map[string]any{"draft_id":""}},
        {"simulate", "invalid digest", map[string]any{"draft_id":draft.ID,"expected_source_digest":123}},
    }
    for _, tc := range cases {
        t.Run(tc.description, func(t *testing.T) {
            response := jsonPost(t, server.URL+"/api/"+tc.endpoint, "test-secret", request(tc.extra))
            response.Body.Close()
            if response.StatusCode != 400 {
                t.Fatalf("%s accepted: HTTP %d", tc.description, response.StatusCode)
            }
        })
    }
    world.mu.Lock()
    gotCalls := len(world.calls)
    world.mu.Unlock()
    if gotCalls != initialCalls {
        t.Fatalf("rejected execution contacted an upstream: before=%d after=%d", initialCalls, gotCalls)
    }

    // A different operator may change the draft after the UI inspected it.
    changed, err := svc.Store.DraftUpdate(draft.ID, draft.Source+"\n# concurrent edit\n")
    if err != nil { t.Fatal(err) }
    response := jsonPost(t, server.URL+"/api/test", "test-secret", request(map[string]any{
        "draft_id": changed.ID, "expected_source_digest": draft.SourceDigest,
    }))
    response.Body.Close()
    if response.StatusCode != 400 { t.Fatalf("stale digest accepted: HTTP %d", response.StatusCode) }

    // Correctly pinned live calls still work.
    live := jsonPost(t, server.URL+"/api/run", "test-secret", request(map[string]any{
        "expected_revision": active.ID,
    }))
    if live.StatusCode != 200 { t.Fatalf("matching active revision rejected: HTTP %d", live.StatusCode) }
    live.Body.Close()

    world.mu.Lock()
    didCall := false
    for _, method := range world.calls {
        if method == "tools/call" { didCall = true }
    }
    world.mu.Unlock()
    if !didCall { t.Fatal("matching live run never reached the MCP upstream") }

    // The current source moved: a stale reviewed revision must not dispatch.
    if _, err := svc.ActivateIfDigest(changed.ID, changed.SourceDigest); err != nil { t.Fatal(err) }
    _, current := svc.Current()
    if current.ID == active.ID { t.Fatal("expected a new activated revision") }
    world.mu.Lock()
    before := len(world.calls)
    world.mu.Unlock()
    response = jsonPost(t, server.URL+"/api/run", "test-secret", request(map[string]any{
        "expected_revision": active.ID,
    }))
    response.Body.Close()
    if response.StatusCode != 400 { t.Fatalf("stale activation accepted: HTTP %d", response.StatusCode) }
    world.mu.Lock()
    after := len(world.calls)
    world.mu.Unlock()
    if after != before { t.Fatal("stale live run dispatched an effect") }

    // Matching dry-run from the exact current draft source still resolves
    // against the reviewed source (the engine may require explicit mocks).
    _, err = svc.Admin(context.Background(),"regate_test", request(map[string]any{
        "draft_id": changed.ID, "expected_source_digest": changed.SourceDigest,
    }))
    if err != nil && (strings.Contains(err.Error(), "source conflict") ||
        strings.Contains(err.Error(), "expected_source_digest")) {
        t.Fatalf("matching draft digest unexpectedly rejected: %v", err)
    }
}
