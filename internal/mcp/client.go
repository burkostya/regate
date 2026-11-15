package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/burkostya/regate/internal/netguard"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const Protocol = "2026-07-28"
const LegacyProtocol = "2025-03-26"
const MaxResponse = 8 << 20

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("MCP %d: %s", e.Code, e.Message) }

type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *RPCError       `json:"error,omitempty"`
}
type Conn struct {
	URL         string
	AuthEnv     string
	Timeout     time.Duration
	Client      *http.Client
	mu          sync.Mutex
	initialized bool
	version     string
	sessionID   string
	counter     atomic.Uint64
}

func NewConn(url, tokenEnv string, timeout time.Duration) *Conn {
	return &Conn{URL: url, AuthEnv: tokenEnv, Timeout: timeout, Client: netguard.Client(timeout)}
}
func meta() map[string]any {
	return map[string]any{"io.modelcontextprotocol/protocolVersion": Protocol, "io.modelcontextprotocol/clientInfo": map[string]any{"name": "regate", "version": "0.1.0"}, "io.modelcontextprotocol/clientCapabilities": map[string]any{}}
}
func wrapParams(p map[string]any, latest bool) map[string]any {
	cp := map[string]any{}
	for k, v := range p {
		cp[k] = v
	}
	if latest {
		cp["_meta"] = meta()
	}
	return cp
}
func (c *Conn) send(ctx context.Context, method string, params map[string]any, version, session string) (json.RawMessage, string, error) {
	id := c.counter.Add(1)
	if params == nil {
		params = map[string]any{}
	}
	p := params
	if version == Protocol {
		p = wrapParams(params, true)
	}
	payload := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": p}
	body, _ := json.Marshal(payload)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if e != nil {
		return nil, "", e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if version == Protocol {
		req.Header.Set("Mcp-Protocol-Version", Protocol)
		req.Header.Set("Mcp-Method", method)
	}
	if version != Protocol && version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if c.AuthEnv != "" {
		if token := os.Getenv(c.AuthEnv); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, e := c.Client.Do(req)
	if e != nil {
		return nil, "", e
	}
	defer resp.Body.Close()
	sessionHeader := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, sessionHeader, fmt.Errorf("HTTP %d from upstream: %s", resp.StatusCode, string(b))
	}
	var raw []byte
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		sc := bufio.NewScanner(io.LimitReader(resp.Body, MaxResponse))
		sc.Buffer(make([]byte, 4096), MaxResponse)
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "data:") {
				dat := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if dat == "" {
					continue
				}
				var check map[string]json.RawMessage
				if json.Unmarshal([]byte(dat), &check) == nil && len(check["id"]) > 0 {
					var got uint64
					_ = json.Unmarshal(check["id"], &got)
					if got == id {
						raw = []byte(dat)
						break
					}
				}
			}
		}
		if e = sc.Err(); e != nil {
			return nil, sessionHeader, e
		}
	} else {
		raw, e = io.ReadAll(io.LimitReader(resp.Body, MaxResponse+1))
		if e != nil {
			return nil, sessionHeader, e
		}
		if len(raw) > MaxResponse {
			return nil, sessionHeader, errors.New("upstream response too large")
		}
	}
	if len(raw) == 0 {
		return nil, sessionHeader, fmt.Errorf("empty response to %s", method)
	}
	var rpc Response
	if e = json.Unmarshal(raw, &rpc); e != nil {
		return nil, sessionHeader, fmt.Errorf("invalid upstream JSON: %w", e)
	}
	if rpc.Error != nil {
		return nil, sessionHeader, rpc.Error
	}
	if rpc.Result == nil {
		return nil, sessionHeader, fmt.Errorf("missing MCP result for %s", method)
	}
	return rpc.Result, sessionHeader, nil
}
func (c *Conn) notify(ctx context.Context, method string) error {
	payload := map[string]any{"jsonrpc": "2.0", "method": method}
	b, _ := json.Marshal(payload)
	req, e := http.NewRequestWithContext(ctx, "POST", c.URL, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	if c.version != "" {
		req.Header.Set("Mcp-Protocol-Version", c.version)
	}
	if c.AuthEnv != "" {
		if tok := os.Getenv(c.AuthEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, e := c.Client.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("notification %s: HTTP %d", method, resp.StatusCode)
	}
	return nil
}
func (c *Conn) initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	// New stateless revision first; if unsupported, fall back to legacy initialize.
	_, _, err := c.send(ctx, "server/discover", map[string]any{}, Protocol, "")
	if err == nil {
		c.version = Protocol
		c.initialized = true
		return nil
	}
	params := map[string]any{"protocolVersion": LegacyProtocol, "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "regate", "version": "0.1.0"}}
	raw, sid, e := c.send(ctx, "initialize", params, "", "")
	if e != nil {
		return fmt.Errorf("discover: %v; initialize: %w", err, e)
	}
	var res map[string]any
	if e = json.Unmarshal(raw, &res); e != nil {
		return e
	}
	version, _ := res["protocolVersion"].(string)
	if version == "" {
		version = LegacyProtocol
	}
	c.version = version
	c.sessionID = sid
	if e = c.notify(ctx, "notifications/initialized"); e != nil {
		return e
	}
	c.initialized = true
	return nil
}
func (c *Conn) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	if err := c.initialize(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	version, sid := c.version, c.sessionID
	c.mu.Unlock()
	result, _, e := c.send(ctx, method, params, version, sid)
	return result, e
}
func (c *Conn) ListTools(ctx context.Context) ([]map[string]any, error) {
	raw, e := c.Call(ctx, "tools/list", map[string]any{})
	if e != nil {
		return nil, e
	}
	var out struct {
		Tools []map[string]any `json:"tools"`
	}
	if e = json.Unmarshal(raw, &out); e != nil {
		return nil, e
	}
	return out.Tools, nil
}
func (c *Conn) CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	raw, e := c.Call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if e != nil {
		return nil, e
	}
	var v map[string]any
	if e = json.Unmarshal(raw, &v); e != nil {
		return nil, e
	}
	return v, nil
}
func (c *Conn) Status() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"url": c.URL, "initialized": c.initialized, "protocol": c.version, "session": c.sessionID != "", "timeout_ms": strconv.FormatInt(c.Timeout.Milliseconds(), 10)}
}
