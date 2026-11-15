// regate-demo is a local fixture MCP server and decision/PII API.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

func main() {
	http.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		var q struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if e := json.NewDecoder(r.Body).Decode(&q); e != nil {
			http.Error(w, e.Error(), 400)
			return
		}
		result := map[string]any{"resultType": "complete"}
		switch q.Method {
		case "server/discover":
			result["supportedVersions"] = []string{"2026-07-28"}
			result["serverInfo"] = map[string]string{"name": "regate-demo", "version": "1"}
			result["capabilities"] = map[string]any{"tools": map[string]any{}}
		case "tools/list":
			result["tools"] = []any{map[string]any{"name": "get_customer", "inputSchema": map[string]any{"type": "object"}}, map[string]any{"name": "summarize", "inputSchema": map[string]any{"type": "object"}}}
		case "tools/call":
			name, _ := q.Params["name"].(string)
			args, _ := q.Params["arguments"].(map[string]any)
			var structured any
			switch name {
			case "get_customer":
				structured = map[string]any{"customer_id": args["customer_id"], "name": "Alice Example", "email": "alice@example.com", "notes": "Contact alice@example.com about the order"}
			case "summarize":
				structured = map[string]any{"summary": fmt.Sprintf("Customer summary: %v", args["notes"]), "redacted_email": args["email"], "safe": true}
			default:
				result["isError"] = true
				result["content"] = []any{map[string]any{"type": "text", "text": "unknown tool"}}
			}
			if structured != nil {
				result["structuredContent"] = structured
				txt, _ := json.Marshal(structured)
				result["content"] = []any{map[string]any{"type": "text", "text": string(txt)}}
			}
		default:
			result["error"] = "not implemented"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": result})
	})
	http.HandleFunc("/decision", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		decision := "allow"
		if strings.HasPrefix(fmt.Sprint(input.Input["customer_id"]), "DENY") {
			decision = "deny"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"decision": decision})
	})
	http.HandleFunc("/redact", func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Input map[string]any `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&input)
		for k, v := range input.Input {
			if k == "email" {
				input.Input[k] = "[REDACTED]"
			} else if text, ok := v.(string); ok {
				input.Input[k] = strings.ReplaceAll(text, "alice@example.com", "[REDACTED]")
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"action": "replace", "value": input.Input})
	})
	log.Println("demo fixture at http://127.0.0.1:9091")
	log.Fatal(http.ListenAndServe("127.0.0.1:9091", nil))
}
