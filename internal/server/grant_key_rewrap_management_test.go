package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGrantKeyManagementIsAdminOnlyAndDoesNotExposeCredentials(t *testing.T) {
	secret := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("g", 32)))
	ring, err := json.Marshal(map[string]any{
		"active": "grant-v1", "keys": map[string]string{"grant-v1": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("REGATE_GRANT_KEYS_JSON", string(ring))
	_, api, _ := fixture(t)

	unauthorized := jsonPost(t, api.URL+"/api/grant-rewrap", "", map[string]any{})
	unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized grant rewrap permitted: HTTP %d", unauthorized.StatusCode)
	}
	anonymousUsage, err := http.Get(api.URL + "/api/grant-key-usage")
	if err != nil {
		t.Fatal(err)
	}
	anonymousUsage.Body.Close()
	if anonymousUsage.StatusCode != http.StatusUnauthorized {
		t.Fatalf("grant key inventory exposed without admin: HTTP %d", anonymousUsage.StatusCode)
	}
	request, err := http.NewRequest(http.MethodGet, api.URL+"/api/grant-key-usage", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer test-secret")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	usage := decode(t, response)
	if usage["active_key"] != "grant-v1" || usage["total"] != float64(0) {
		t.Fatalf("admin inventory incorrect: %#v", usage)
	}
	report := decode(t, jsonPost(t, api.URL+"/api/grant-rewrap",
		"test-secret", map[string]any{}))
	if report["rewrapped"] != float64(0) ||
		report["remaining_old_key"] != float64(0) ||
		report["active_key"] != "grant-v1" {
		t.Fatalf("empty grant rewrap unexpected: %#v", report)
	}
	rpc := decode(t, latestCall(t, api.URL+"/mcp/admin", "test-secret",
		"tools/call", 491, map[string]any{
			"name": "regate_grant_rewrap", "arguments": map[string]any{},
		}))
	if rpc["error"] != nil || rpc["result"] == nil {
		t.Fatalf("admin MCP grant rewrap unavailable: %#v", rpc)
	}
	serialized, err := json.Marshal([]any{usage, report, rpc})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), secret) {
		t.Fatal("grant key bytes leaked in admin response")
	}
	public := latestCall(t, api.URL+"/mcp/support", "",
		"tools/call", 492, map[string]any{
			"name": "regate_grant_rewrap", "arguments": map[string]any{},
		})
	defer public.Body.Close()
	data, err := io.ReadAll(public.Body)
	if err != nil {
		t.Fatal(err)
	}
	if public.StatusCode == http.StatusOK {
		var envelope map[string]any
		if err := json.Unmarshal(data, &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope["error"] == nil {
			result, ok := envelope["result"].(map[string]any)
			if ok && result["isError"] != true {
				t.Fatal("public MCP invoked privileged grant rotation")
			}
		}
	}
}
