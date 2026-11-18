// Package testmcp is a deliberately loopback-only, destructive-free MCP
// provider used by Regate's two-process acceptance stand. It is NOT a
// production provider or a general-purpose database.
package testmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/hex"
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
	// Operation tokens are persisted atomically with values. They cannot
	// be set by a subsequent read or a separate management operation.
	OperationTokens map[string]string `json:"operation_tokens,omitempty"`
	WritesReceived int64             `json:"writes_received"`
	WritesCommitted int64            `json:"writes_committed"`
	// The provider's own durable monotone generation. Only the current
	// generation can mutate a record; an old worker cannot write after
	// a newer generation has been observed by this provider.
	FenceEpoch uint64 `json:"fence_epoch,omitempty"`
	FencedRejections int64 `json:"fenced_rejections,omitempty"`
	IdempotentReplays int64 `json:"idempotent_replays,omitempty"`
	// An inverse is an irreversible, durable tombstone, not a deletion of
	// source evidence. Original record + operation token remain readable.
	Compensated map[string]bool `json:"compensated,omitempty"`
	CompensationsReceived int64 `json:"compensations_received,omitempty"`
	CompensationsCommitted int64 `json:"compensations_committed,omitempty"`
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

func validOperationToken(token string) bool {
	if token=="" {return true}
	if len(token)!=64 || strings.ToLower(token)!=token {return false}
	_,err:=hex.DecodeString(token)
	return err==nil
}

func New(instance, dir string) (*Server, error) {
	if !validID(instance) || dir == "" {
		return nil, errors.New("fixture requires a safe instance name and dedicated state directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := &Server{instance: instance, path: filepath.Join(dir, "records.json")}
	s.state = diskState{Instance: instance, Records: map[string]string{},
		OperationTokens: map[string]string{}, Compensated: map[string]bool{}}
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
		restored.WritesReceived < 0 || restored.WritesCommitted < 0 ||
		restored.FencedRejections < 0 || restored.IdempotentReplays < 0 ||
		restored.CompensationsReceived < 0 || restored.CompensationsCommitted < 0 ||
		restored.CompensationsCommitted > restored.CompensationsReceived {
		return nil, errors.New("test MCP state belongs to another instance or is invalid")
	}
	for id, value := range restored.Records {
		if !validID(id) || len(value) > 256 || !utf8.ValidString(value) {
			return nil, errors.New("invalid persisted test MCP record")
		}
	}
	seenTokens := map[string]bool{}
	for id, token := range restored.OperationTokens {
		if _, exists := restored.Records[id]; !exists || token == "" ||
			!validOperationToken(token) || seenTokens[token] {
			return nil, errors.New("invalid or reused persisted test MCP operation token")
		}
		seenTokens[token] = true
	}
	for id, compensated := range restored.Compensated {
		if !compensated || restored.OperationTokens[id] == "" {
			return nil, errors.New("invalid persisted test MCP compensation")
		}
	}
	if len(restored.Compensated) != int(restored.CompensationsCommitted) {
		return nil, errors.New("inconsistent persisted compensation count")
	}
	if restored.Compensated == nil { restored.Compensated = map[string]bool{} }
	if restored.OperationTokens == nil { restored.OperationTokens = map[string]string{} }
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

// copyState is called with the lock held. Mutating a copy ensures failed
// fsync does not accidentally alter the in-memory committed state.
func (s *Server) copyState() diskState {
    next := s.state
    next.Records = make(map[string]string,len(s.state.Records))
    for k,v:=range s.state.Records {next.Records[k]=v}
    next.OperationTokens = make(map[string]string,len(s.state.OperationTokens))
    for k,v:=range s.state.OperationTokens {next.OperationTokens[k]=v}
    next.Compensated = make(map[string]bool,len(s.state.Compensated))
    for k,v:=range s.state.Compensated {next.Compensated[k]=v}
    return next
}

// advanceFence is explicit fixture-only provider takeover. The generation is
// atomically persisted with records; lower generations cannot mutate after it.
// A production adapter would need authenticated promotion and bound tokens.
func (s *Server) advanceFence(epoch uint64) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if epoch == 0 || epoch <= s.state.FenceEpoch {
        return errors.New("fence generation must increase")
    }
    next:=s.copyState()
    next.FenceEpoch=epoch
    if err:=s.persist(next);err!=nil{return err}
    s.state=next
    return nil
}

// put implements native provider idempotency keyed by operation token AND
// immutable record identity/value. A reused token for a different record,
// changed payload, or compensated record is never a successful replay.
func (s *Server) put(id, value, token string, epoch uint64) (bool, error) {
    s.mu.Lock()
    defer s.mu.Unlock()
    next:=s.copyState()
    next.WritesReceived++
    if epoch != next.FenceEpoch {
        next.FencedRejections++
        if err:=s.persist(next);err!=nil{return false,err}
        s.state=next
        return false,errors.New("stale provider fence generation")
    }
    if previous, found:=next.Records[id];found {
        if token!="" && next.OperationTokens[id]==token &&
            previous==value && !next.Compensated[id] {
            next.IdempotentReplays++
            if err:=s.persist(next);err!=nil{return false,err}
            s.state=next
            return true,nil
        }
        if err:=s.persist(next);err!=nil{return false,err}
        s.state=next
        return false,errors.New("record already exists with a different effect or was compensated")
    }
    for _,prior:=range next.OperationTokens {
        if token!="" && prior==token {
            if err:=s.persist(next);err!=nil{return false,err}
            s.state=next
            return false,errors.New("operation token already used for another record")
        }
    }
    if len(next.Records)>=maxRecordCount {
        return false,errors.New("fixture record capacity reached")
    }
    next.Records[id]=value
    if token!="" {next.OperationTokens[id]=token}
    next.WritesCommitted++
    if err:=s.persist(next);err!=nil{return false,err}
    s.state=next
    return false,nil
}

func (s *Server) revoke(id string,epoch uint64) error {
    s.mu.Lock()
    defer s.mu.Unlock()
    next:=s.copyState()
    next.CompensationsReceived++
    if epoch!=next.FenceEpoch {
        next.FencedRejections++
        if err:=s.persist(next);err!=nil{return err}
        s.state=next
        return errors.New("stale provider fence generation")
    }
    if _,exists:=next.Records[id];!exists || next.OperationTokens[id]=="" ||
        next.Compensated[id] {
        if err:=s.persist(next);err!=nil{return err}
        s.state=next
        return errors.New("compensation requires a live immutable token-bound record")
    }
    next.Compensated[id]=true
    next.CompensationsCommitted++
    if err:=s.persist(next);err!=nil{return err}
    s.state=next
    return nil
}

func (s *Server) lookup(id string) (string,string,bool,bool) {
    s.reads.Add(1)
    s.mu.Lock()
    defer s.mu.Unlock()
    v,ok:=s.state.Records[id]
    return v,s.state.OperationTokens[id],s.state.Compensated[id],ok
}
func (s *Server) has(id string) bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    _,ok:=s.state.Records[id]
    return ok
}
func (s *Server) hasCompensated(id string) bool {
    s.mu.Lock()
    defer s.mu.Unlock()
    return s.state.Compensated[id]
}
func (s *Server) stats() map[string]any {
    s.mu.Lock()
    defer s.mu.Unlock()
    return map[string]any{
        "instance":s.instance,
        "writes_received":s.state.WritesReceived,
        "writes_committed":s.state.WritesCommitted,
        "idempotent_replays":s.state.IdempotentReplays,
        "fence_epoch":s.state.FenceEpoch,
        "fenced_rejections":s.state.FencedRejections,
        "compensations_received":s.state.CompensationsReceived,
        "compensations_committed":s.state.CompensationsCommitted,
        "record_count":len(s.state.Records),
        "reads":s.reads.Load(),
    }
}

type putArgs struct {
	RecordID string `json:"record_id"`
	Value    string `json:"value"`
	OperationToken string `json:"operation_token,omitempty"`
	FenceEpoch uint64 `json:"fence_epoch,omitempty"`
	Fault    string `json:"fault,omitempty"`
}
type getArgs struct {
	RecordID string `json:"record_id"`
}
type revokeArgs struct {
    RecordID string `json:"record_id"`
    FenceEpoch uint64 `json:"fence_epoch,omitempty"`
    Fault string `json:"fault,omitempty"`
}
type fenceArgs struct {
    FenceEpoch uint64 `json:"fence_epoch"`
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

func recordResult(instance,id,value,token string,compensated,replay bool) *sdk.CallToolResult {
    record:=map[string]any{"record_id":id,"value":value,"instance":instance,
        "compensated":compensated}
    if token!="" {record["operation_token"]=token}
    body:=map[string]any{"found":true,"record":record}
    if replay {body["idempotent_replay"]=true}
    return &sdk.CallToolResult{
        StructuredContent:body,
        Content:[]sdk.Content{&sdk.TextContent{Text:"test record available"}},
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
				"operation_token": map[string]any{"type": "string", "pattern": "^[0-9a-f]{64}$"},
                "fence_epoch": map[string]any{"type":"integer","minimum":0},
				"fault": map[string]any{"type": "string", "enum": []any{"", "drop_after_commit", "error_after_commit"}},
			},
			"required": []string{"record_id", "value"},
		},
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var a putArgs
		if err := decodeArgs(req.Params.Arguments, &a); err != nil ||
			!validID(a.RecordID) || len(a.Value) > 256 || !utf8.ValidString(a.Value) ||
			!validOperationToken(a.OperationToken) ||
			(a.Fault != "" && a.Fault != "drop_after_commit" && a.Fault != "error_after_commit") {
			return rejected("invalid bounded test write"), nil
		}
		replay, err := s.put(a.RecordID, a.Value,a.OperationToken,a.FenceEpoch)
        if err != nil { return rejected("fixture write rejected: "+err.Error()),nil }
		if a.Fault=="error_after_commit" && !replay {
			// Intentionally return MCP isError AFTER the record was fsynced.
			return rejected("test-only post-commit application error"),nil
		}
		return recordResult(s.instance,a.RecordID,a.Value,a.OperationToken,false,replay),nil
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
		if v,token,compensated,ok := s.lookup(a.RecordID);ok {
            return recordResult(s.instance,a.RecordID,v,token,compensated,false),nil
        }
		// Not found NEVER attests that a remote write did not happen.
		return &sdk.CallToolResult{
			StructuredContent: map[string]any{"found": false, "instance": s.instance},
			Content: []sdk.Content{&sdk.TextContent{Text: "test record not found"}},
		}, nil
	})
	m.AddTool(&sdk.Tool{
        Name:"advance_fence", Description:"TEST-ONLY provider generation promotion; strictly monotone",
        InputSchema:map[string]any{"type":"object","additionalProperties":false,
            "properties":map[string]any{"fence_epoch":map[string]any{"type":"integer","minimum":1}},
            "required":[]string{"fence_epoch"}},
    },func(_ context.Context,req *sdk.CallToolRequest)(*sdk.CallToolResult,error){
        var a fenceArgs
        if err:=decodeArgs(req.Params.Arguments,&a);err!=nil || a.FenceEpoch==0 {
            return rejected("invalid provider fence"),nil
        }
        if err:=s.advanceFence(a.FenceEpoch);err!=nil {return rejected("stale fence promotion"),nil}
        return &sdk.CallToolResult{
            StructuredContent:map[string]any{"fence_epoch":a.FenceEpoch,"instance":s.instance},
            Content:[]sdk.Content{&sdk.TextContent{Text:"fixture fence advanced"}},
        },nil
    })
    m.AddTool(&sdk.Tool{
        Name:"revoke_record",Description:"TEST-ONLY durable compensating tombstone; record history remains",
        InputSchema:map[string]any{"type":"object","additionalProperties":false,
            "properties":map[string]any{
                "record_id":map[string]any{"type":"string","minLength":1,"maxLength":48},
                "fence_epoch":map[string]any{"type":"integer","minimum":0},
                "fault":map[string]any{"type":"string",
                    "enum":[]any{"","drop_after_commit","error_after_commit"}},
            },"required":[]string{"record_id"}},
    },func(_ context.Context,req *sdk.CallToolRequest)(*sdk.CallToolResult,error){
        var a revokeArgs
        if err:=decodeArgs(req.Params.Arguments,&a);err!=nil ||
            !validID(a.RecordID) ||
            (a.Fault!="" && a.Fault!="drop_after_commit" && a.Fault!="error_after_commit") {
            return rejected("invalid test compensation"),nil
        }
        if err:=s.revoke(a.RecordID,a.FenceEpoch);err!=nil {
            return rejected("fixture compensation rejected: "+err.Error()),nil
        }
        if a.Fault=="error_after_commit" {
            return rejected("test-only post-compensation application error"),nil
        }
        return &sdk.CallToolResult{
            StructuredContent:map[string]any{"record_id":a.RecordID,
                "instance":s.instance,"compensated":true},
            Content:[]sdk.Content{&sdk.TextContent{Text:"fixture tombstone committed"}},
        },nil
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
		drop := frame.Method == "tools/call" &&
            (frame.Params.Name=="put_record" || frame.Params.Name=="revoke_record") &&
            frame.Params.Arguments.Fault=="drop_after_commit"
		if !drop {
			sdkHandler.ServeHTTP(w, r)
			return
		}
		captured := httptest.NewRecorder()
		sdkHandler.ServeHTTP(captured, r)
		// Do not drop a schema-invalid or uncommitted write response.
		committed := s.has(frame.Params.Arguments.RecordID)
        if frame.Params.Name=="revoke_record" {committed=s.hasCompensated(frame.Params.Arguments.RecordID)}
        if captured.Code==http.StatusOK &&
            !bytes.Contains(captured.Body.Bytes(),[]byte("\"isError\":true")) && committed {
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
