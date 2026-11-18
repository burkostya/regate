// Package testmcp is a deliberately loopback-only, destructive-free MCP
// provider used by Regate's two-process acceptance stand. It is NOT a
// production provider or a general-purpose database.
package testmcp

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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxRecordCount = 128
const maxWireBytes = 4 << 20

type diskState struct {
	Instance       string            `json:"instance"`
	Records        map[string]string `json:"records"`
	WritesReceived int64             `json:"writes_received"`
	WritesCommitted int64            `json:"writes_committed"`
}

// Server has private process-local memory and a private fsynced JSON file.
// No database, credential or network outside the designated test directory
// is accessed. Test applications may restart a server with the same state.
type Server struct {
	instance string
	path     string
	mu       sync.Mutex
	state    diskState
	reads    atomic.Int64
}

func validID(v string) bool {
	if len(v) == 0 || len(v) > 48 {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			if r < 'A' || r > 'Z' {
				if r < 'a' || r > 'z' {
					if r != '_' && r != '-' {
						return false
					}
				}
			}
		}
	}
	return true
}

func New(instance, dir string) (*Server, error) {
	if !validID(instance) || dir == "" {
		return nil, errors.New("fixture requires a safe instance name and dedicated state directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Server{instance: instance, path: filepath.Join(dir, "records.json")}
	s.state = diskState{Instance: instance, Records: map[string]string{}}
	body, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	case len(body) > 1<<20:
		return nil, errors.New("test MCP state exceeds safe fixture limit")
	}
	var restored diskState
	if err := json.Unmarshal(body, &restored); err != nil {
		return nil, fmt.Errorf("invalid persisted fixture state: %w", err)
	}
	if restored.Instance != instance || restored.Records == nil ||
		len(restored.Records) > maxRecordCount ||
		restored.WritesCommitted > restored.WritesReceived ||
		restored.WritesReceived < 0 || restored.WritesCommitted < 0 {
		return nil, errors.New("test MCP state belongs to another instance or is invalid")
	}
	for id, value := range restored.Records {
		if !validID(id) || len(value) > 256 || !utf8.ValidString(value) {
			return nil, errors.New("invalid persisted test MCP record")
		}
	}
	s.state = restored
	return s, nil
}

// persist replaces one complete state atomically. Failure does not alter
// process memory; fsync file + containing directory makes restart assertions
// meaningful even after an abrupt process termination.
func (s *Server) persist(next diskState) error {
	body, err := json.Marshal(next)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".fixture-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (s *Server) put(id, value string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := s.state
	next.Records = make(map[string]string, len(s.state.Records)+1)
	for k, v := range s.state.Records {
		next.Records[k] = v
	}
	next.WritesReceived++
	_, found := next.Records[id]
	if !found && len(next.Records) >= maxRecordCount {
		return false, errors.New("fixture record capacity reached")
	}
	if !found {
		next.Records[id] = value
		next.WritesCommitted++
	}
	if err := s.persist(next); err != nil {
		return false, err
	}
	s.state = next
	if found {
		return false, errors.New("duplicate record ID; fixture never silently overwrites a write")
	}
	return true, nil
}

func (s *Server) lookup(id string) (string, bool) {
	s.reads.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.state.Records[id]
	return v, ok
}

func (s *Server) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.state.Records[id]
	return ok
}

func (s *Server) stats() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"instance": s.instance,
		"writes_received": s.state.WritesReceived,
		"writes_committed": s.state.WritesCommitted,
		"record_count": len(s.state.Records),
		"reads": s.reads.Load(),
	}
}

type putArgs struct {
	RecordID string `json:"record_id"`
	Value    string `json:"value"`
	Fault    string `json:"fault,omitempty"`
}
type getArgs struct {
	RecordID string `json:"record_id"`
}

func decodeArgs(value any, out any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func rejected(reason string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: reason}},
	}
}

func recordResult(instance, id, value string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		StructuredContent: map[string]any{
			"found": true,
			"record": map[string]any{
				"record_id": id, "value": value, "instance": instance,
			},
		},
		Content: []sdk.Content{&sdk.TextContent{Text: "test record available"}},
	}
}

func (s *Server) mcpServer() *sdk.Server {
	m := sdk.NewServer(&sdk.Implementation{Name: "regate-test-" + s.instance, Version: "1.0"}, nil)
	m.AddTool(&sdk.Tool{
		Name: "put_record", Description: "Write a bounded fixture-only immutable record",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"record_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 48},
				"value": map[string]any{"type": "string", "maxLength": 256},
				"fault": map[string]any{"type": "string", "enum": []any{"", "drop_after_commit"}},
			},
			"required": []string{"record_id", "value"},
		},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var a putArgs
		if err := decodeArgs(req.Params.Arguments, &a); err != nil ||
			!validID(a.RecordID) || len(a.Value) > 256 || !utf8.ValidString(a.Value) ||
			(a.Fault != "" && a.Fault != "drop_after_commit") {
			return rejected("invalid bounded test write"), nil
		}
		if _, err := s.put(a.RecordID, a.Value); err != nil {
			return rejected("fixture write rejected"), nil
		}
		return recordResult(s.instance, a.RecordID, a.Value), nil
	})
	m.AddTool(&sdk.Tool{
		Name: "get_record", Description: "Read a fixture record without modifying the state",
		InputSchema: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"record_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 48},
			},
			"required": []string{"record_id"},
		},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var a getArgs
		if err := decodeArgs(req.Params.Arguments, &a); err != nil || !validID(a.RecordID) {
			return rejected("invalid test read"), nil
		}
		if v, ok := s.lookup(a.RecordID); ok {
			return recordResult(s.instance, a.RecordID, v), nil
		}
		// Not found NEVER attests that a remote write did not happen.
		return &sdk.CallToolResult{
			StructuredContent: map[string]any{"found": false, "instance": s.instance},
			Content: []sdk.Content{&sdk.TextContent{Text: "test record not found"}},
		}, nil
	})
	m.AddTool(&sdk.Tool{
		Name: "stats", Description: "Read fixture write-attempt and commit counters",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
	}, func(_ context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{
			StructuredContent: s.stats(),
			Content: []sdk.Content{&sdk.TextContent{Text: "fixture counters"}},
		}, nil
	})
	m.AddResource(&sdk.Resource{
		URI: "fixture://" + s.instance + "/guide",
		Name: s.instance + "_guide", MIMEType: "text/plain",
	}, func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{
			Contents: []*sdk.ResourceContents{{
				URI: req.Params.URI, MIMEType: "text/plain",
				Text: "Test-only isolated MCP instance: " + s.instance,
			}},
		}, nil
	})
	m.AddPrompt(&sdk.Prompt{Name: s.instance + "_review", Description: "Review test data"}, func(_ context.Context, _ *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		return &sdk.GetPromptResult{
			Messages: []*sdk.PromptMessage{{
				Role: "user", Content: &sdk.TextContent{Text: "Review only synthetic " + s.instance + " fixture records"},
			}},
		}, nil
	})
	return m
}

type faultEnvelope struct {
	Method string `json:"method"`
	Params struct {
		Name string `json:"name"`
		Arguments putArgs `json:"arguments"`
	} `json:"params"`
}

// Handler intentionally closes the HTTP connection only AFTER the official
// SDK executes the put and the file is fsynced. This tests an ambiguous
// provider write response, not a simulated failure before execution.
func (s *Server) Handler() http.Handler {
	// Keep one immutable SDK catalog per fixture process. All requests
	// share this server; each client session remains independently stateless.
	sdkServer := s.mcpServer()
	sdkHandler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return sdkServer
	}, &sdk.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true,
		MaxRequestBodyBytes: maxWireBytes,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"instance": s.instance, "status": "ready"})
			return
		case "/decision":
			s.decision(w, r)
			return
		case "/mcp":
			// Continue with the official MCP Go SDK.
		default:
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			sdkHandler.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxWireBytes+1))
		if err != nil || len(body) > maxWireBytes {
			http.Error(w, "invalid fixture MCP body", http.StatusRequestEntityTooLarge)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var frame faultEnvelope
		_ = json.Unmarshal(body, &frame)
		drop := frame.Method == "tools/call" && frame.Params.Name == "put_record" &&
			frame.Params.Arguments.Fault == "drop_after_commit"
		if !drop {
			sdkHandler.ServeHTTP(w, r)
			return
		}
		captured := httptest.NewRecorder()
		sdkHandler.ServeHTTP(captured, r)
		// Do not drop a schema-invalid or uncommitted write response.
		if captured.Code == http.StatusOK && !bytes.Contains(captured.Body.Bytes(), []byte("\"isError\":true")) &&
			s.has(frame.Params.Arguments.RecordID) {
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					_ = conn.Close()
					return
				}
			}
			http.Error(w, "fixture deliberately lost the post-commit response", http.StatusBadGateway)
			return
		}
		for key, list := range captured.Header() {
			for _, value := range list {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(captured.Code)
		_, _ = w.Write(captured.Body.Bytes())
	})
}

func (s *Server) decision(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<10+1))
	if err != nil || len(body) > 16<<10 {
		http.Error(w, "invalid test decision request", http.StatusBadRequest)
		return
	}
	var input struct {
		Input map[string]any `json:"input"`
	}
	decision := "deny"
	if json.Unmarshal(body, &input) == nil {
		if id, ok := input.Input["record_id"].(string); ok &&
			validID(id) && !strings.HasPrefix(id, "DENY-") {
			decision = "allow"
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"decision": decision})
}

// Serve is safe only with a loopback listener and dedicated fixture data.
// It allows a graceful CLI shutdown; process-kill scenarios are tested using
// the persisted file before a new server is launched.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		return errors.New("test MCP server refuses non-loopback listeners")
	}
	h := &http.Server{
		Handler: s.Handler(), ReadHeaderTimeout: 5*time.Second,
		IdleTimeout: 30*time.Second, MaxHeaderBytes: 16 << 10,
	}
	go func() {
		<-ctx.Done()
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = h.Shutdown(stop)
	}()
	err := h.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
