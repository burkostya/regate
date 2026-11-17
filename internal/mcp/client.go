// Package mcp connects Regate to upstream MCP servers over HTTP.
//
// Protocol negotiation, JSON-RPC envelopes, SSE framing and legacy session
// support are delegated to the official MCP Go SDK. Regate deliberately does
// not support stdio transports.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/burkostya/regate/internal/netguard"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const Protocol = "2026-07-28"
const LegacyProtocol = "2025-03-26"
// MaxRequest bounds fully encoded outgoing MCP HTTP bodies, before network I/O.
const MaxRequest = 4 << 20
const MaxResponse = 8 << 20
const maxToolPages = 100

type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("MCP %d: %s", e.Code, e.Message) }

// Conn is a stateless adapter over the official MCP client. Each operation
// receives a fresh protocol session, which is always closed before returning.
// This makes concurrent requests and reconnects independent and prevents a
// failed write from being replayed on a stale connection. It trades an extra
// discovery/initialization request per operation for predictable lifetimes.
//
// The Client field is kept for compatibility with existing Regate callers;
// its transport is fail-closed and DNS-pinned by the netguard package.
type Conn struct {
	URL     string
	AuthEnv string
	Timeout time.Duration
	Client  *http.Client

	mu          sync.RWMutex
	initialized bool
	version     string
}

func NewConn(endpoint, tokenEnv string, timeout time.Duration) *Conn {
	client := netguard.Client(timeout)
	client.Transport = credentialTransport{base: client.Transport, tokenEnv: tokenEnv}
	return &Conn{URL: endpoint, AuthEnv: tokenEnv, Timeout: timeout, Client: client}
}

// NewConnWithToken is ephemeral: never cache it in a global connection pool.
// The token must be issued for this specific upstream audience, not forwarded
// from the incoming Regate resource-server request.
func NewConnWithToken(endpoint, token string, timeout time.Duration) *Conn {
    conn:=NewConn(endpoint,"",timeout)
    conn.Client.Transport=credentialTransport{base:conn.Client.Transport,staticToken:token}
    return conn
}

// credentialTransport applies credentials only to validated outbound HTTP
// requests; redirect following is already disabled by netguard.Client. This
// is deliberately not a general-purpose OAuth implementation.
type credentialTransport struct {
	base http.RoundTripper
	tokenEnv string
    staticToken string
}

func (t credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// SDK-generated tool arguments and transformed pipeline outputs must not
	// bypass the outbound budget merely because the inbound frame was bounded.
	// Buffer at most one byte beyond the budget, before any connection attempt.
	if clone.Body != nil && clone.Body != http.NoBody {
		body, readErr := io.ReadAll(io.LimitReader(clone.Body, MaxRequest+1))
		closeErr := clone.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, errors.New("cannot read upstream MCP request body")
		}
		if len(body) > MaxRequest {
			return nil, fmt.Errorf("upstream MCP request exceeds %d bytes", MaxRequest)
		}
		clone.Body = io.NopCloser(bytes.NewReader(body))
		clone.ContentLength = int64(len(body))
		clone.TransferEncoding = nil
		// A non-idempotent MCP call must not become transparently replayable.
		clone.GetBody = nil
	}
	if t.staticToken!="" {
        clone.Header.Set("Authorization","Bearer "+t.staticToken)
    } else if t.tokenEnv != "" {
		if token := os.Getenv(t.tokenEnv); token != "" {
			clone.Header.Set("Authorization", "Bearer "+token)
		}
	}
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if err := boundAndValidateMCPResponse(response); err != nil {
		return nil, err
	}
	return response, nil
}

// limitedBody makes oversized JSON and SSE responses fail rather than consume
// unbounded memory. Exactly MaxResponse bytes is accepted, but a larger body
// produces an error on the first over-limit read.
type limitedBody struct {
	io.ReadCloser
	count int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.count > MaxResponse {
		return 0, errors.New("upstream MCP response exceeds 8 MiB")
	}
	n, err := b.ReadCloser.Read(p)
	b.count += int64(n)
	if b.count > MaxResponse {
		return 0, errors.New("upstream MCP response exceeds 8 MiB")
	}
	return n, err
}

func (c *Conn) connect(ctx context.Context) (*sdk.ClientSession, error) {
	if err := netguard.ValidateURL(c.URL); err != nil {
		return nil, fmt.Errorf("invalid upstream MCP URL: %w", err)
	}
	transport := &sdk.StreamableClientTransport{
		Endpoint:             c.URL,
		HTTPClient:           c.Client,
		MaxRetries:           -1,   // an uncertain write outcome must never be retried automatically
		DisableStandaloneSSE: true, // short-lived requests do not need persistent subscriptions
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "regate", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("connect upstream MCP %s: %w", c.URL, err)
	}
	c.mu.Lock()
	c.initialized = true
	if init := session.InitializeResult(); init != nil {
		c.version = init.ProtocolVersion
	}
	c.mu.Unlock()
	return session, nil
}

func withSession[T any](c *Conn, ctx context.Context, invoke func(*sdk.ClientSession) (T, error)) (T, error) {
	var zero T
	// Bound the entire operation, including negotiation and all list pages.
	// The HTTP client timeout alone would only limit individual requests.
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	session, err := c.connect(ctx)
	if err != nil {
		return zero, err
	}
	defer session.Close()
	return invoke(session)
}

// ListTools collects all pages, not just the first page. Repeated cursors and
// unexpectedly large page sequences are rejected to avoid infinite loops when
// an upstream violates pagination rules.
func (c *Conn) ListTools(ctx context.Context) ([]map[string]any, error) {
    return withSession(c, ctx, func(session *sdk.ClientSession) ([]map[string]any,error) {
        return allTools(ctx,session)
    })
}

func remarshal(value any, out any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *Conn) CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	return withSession(c, ctx, func(session *sdk.ClientSession) (map[string]any, error) {
		if args == nil {
			args = map[string]any{}
		}
		result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			return nil, err
		}
		var out map[string]any
		if err := remarshal(result, &out); err != nil {
			return nil, err
		}
		return out, nil
	})
}

// Call provides the existing Regate abstraction for standard resource and
// prompt operations, while routing their wire format through the official SDK.
// Unknown custom methods are rejected until explicitly registered with the
// SDK rather than falling back to a second hand-written JSON-RPC transport.
func (c *Conn) Call(ctx context.Context, method string, params map[string]any) (json.RawMessage, error) {
	return withSession(c, ctx, func(session *sdk.ClientSession) (json.RawMessage, error) {
		var result any
		var err error
		switch method {
		case "resources/read":
			uri, _ := params["uri"].(string)
			if uri == "" {
				return nil, errors.New("resources/read requires uri")
			}
			result, err = session.ReadResource(ctx, &sdk.ReadResourceParams{URI: uri})
		case "resources/list":
			cursor, _ := params["cursor"].(string)
			result, err = session.ListResources(ctx, &sdk.ListResourcesParams{Cursor: cursor})
		case "resources/templates/list":
			cursor, _ := params["cursor"].(string)
			result, err = session.ListResourceTemplates(ctx, &sdk.ListResourceTemplatesParams{Cursor: cursor})
		case "prompts/list":
			cursor, _ := params["cursor"].(string)
			result, err = session.ListPrompts(ctx, &sdk.ListPromptsParams{Cursor: cursor})
		case "prompts/get":
			var p sdk.GetPromptParams
			if err := remarshal(params, &p); err != nil {
				return nil, err
			}
			result, err = session.GetPrompt(ctx, &p)
		case "ping":
			err = session.Ping(ctx, nil)
			result = map[string]any{}
		default:
			return nil, fmt.Errorf("unsupported MCP upstream method %q", method)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	})
}

func (c *Conn) Status() map[string]any {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]any{
		"url":         c.URL,
		"initialized": c.initialized,
		"protocol":    c.version,
		"session":     false, // session is scoped to each operation and closed afterwards
		"timeout_ms":  strconv.FormatInt(c.Timeout.Milliseconds(), 10),
		"transport":   "official-go-sdk",
	}
}
