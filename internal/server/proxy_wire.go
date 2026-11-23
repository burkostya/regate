package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/burkostya/regate/internal/mcp"
)

type proxyEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type proxyCall struct {
	proxyEnvelope
	body          []byte
	internal      bool
	version       string
	methodMatches bool
}

func readProxyCall(r *http.Request) (proxyCall, error) {
	if r.Method != http.MethodPost {
		return proxyCall{}, nil
	}
	var call proxyCall
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxInboundBodyBytes))
	if err != nil {
		return call, err
	}
	if err := checkUniqueJSONKeys(body); err != nil {
		return call, err
	}
	switch bytes.TrimSpace(body)[0] {
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			return call, err
		}
		for key := range fields {
			switch strings.ToLower(key) {
			case "method", "jsonrpc", "id", "result", "error":
				if key != strings.ToLower(key) {
					return call, errors.New("ambiguous MCP envelope")
				}
			}
		}
		if err := json.Unmarshal(body, &call.proxyEnvelope); err != nil {
			return call, err
		}
		methodHeaders := r.Header.Values("Mcp-Method")
		call.methodMatches = len(methodHeaders) == 0 || len(methodHeaders) == 1 && methodHeaders[0] == call.Method
	case '[':
		// Older MCP versions allow batches. Treat them as opaque, potentially
		// effectful requests and never replay them.
	default:
		return call, errors.New("invalid MCP envelope")
	}
	call.body = body
	return call, nil
}

func internalProxyCall(method string, withID bool) proxyCall {
	call := proxyCall{internal: true, proxyEnvelope: proxyEnvelope{JSONRPC: "2.0", Method: method}}
	if withID {
		call.ID, _ = json.Marshal(rand.Text())
	}
	call.body, _ = json.Marshal(call.proxyEnvelope)
	return call
}

func (c proxyCall) request(method string) bool {
	return c.isMethod(method) && len(c.ID) != 0 && !bytes.Equal(c.ID, []byte("null"))
}

func (c proxyCall) isMethod(method string) bool {
	return c.methodMatches && c.JSONRPC == "2.0" && c.Method == method
}

func (c proxyCall) replayable() bool {
	if !c.request(c.Method) {
		return false
	}
	switch c.Method {
	case "ping", "tools/list", "resources/list", "resources/templates/list", "resources/read", "prompts/list", "prompts/get":
		return true
	default:
		return false
	}
}

func (c proxyCall) callback() bool {
	return c.request("") && ((len(c.Result) != 0) != (c.Error != nil))
}

type proxyReply struct {
	status  int
	header  http.Header
	body    []byte
	stream  io.ReadCloser
	lines   *bufio.Scanner
	prefix  []byte
	expired bool
}

func (p *publishedMCP) send(r *http.Request, token string, call proxyCall, binding proxyBinding) (*proxyReply, error) {
	target := *p.target
	method := r.Method
	if call.internal {
		method = http.MethodPost
	}
	if target.RawQuery == "" {
		target.RawQuery = r.URL.RawQuery
	} else if r.URL.RawQuery != "" {
		params, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			return nil, errors.New("invalid MCP query")
		}
		configured, err := url.ParseQuery(target.RawQuery)
		if err != nil {
			return nil, errors.New("invalid upstream endpoint query")
		}
		for key, values := range configured {
			params[key] = values
		}
		target.RawQuery = params.Encode()
	}
	var body io.Reader
	if len(call.body) != 0 {
		// A non-rewindable body keeps the transport from replaying a request
		// when it cannot establish whether the previous attempt was sent.
		body = io.NopCloser(bytes.NewReader(call.body))
	}
	req, err := http.NewRequestWithContext(r.Context(), method, target.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(call.body))
	}
	req.Header = proxyHeaders(r.Header)
	for _, key := range []string{"Authorization", "Cookie", "Content-Length", "Forwarded", "X-Http-Method-Override",
		"X-Original-Url", "X-Rewrite-Url", "X-Real-Ip", proxySessionHeader} {
		req.Header.Del(key)
	}
	for key := range req.Header {
		if strings.HasPrefix(strings.ToLower(key), "x-forwarded-") {
			req.Header.Del(key)
		}
	}
	req.Header.Set("Accept-Encoding", "identity")
	if call.internal {
		version := call.version
		if version == "" && call.Method != "initialize" {
			version = r.Header.Get("Mcp-Protocol-Version")
		}
		req.Header = http.Header{
			"Content-Type":    {"application/json"},
			"Accept":          {"application/json, text/event-stream"},
			"Accept-Encoding": {"identity"},
			"Mcp-Method":      {call.Method},
		}
		if version != "" {
			req.Header.Set("Mcp-Protocol-Version", version)
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			req.Header.Set("Origin", origin)
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if binding.id != "" {
		req.Header.Set(proxySessionHeader, binding.id)
	}
	if call.internal || binding.generation != 0 {
		cursor := req.Header.Get("Last-Event-Id")
		req.Header.Del("Last-Event-Id")
		if !call.internal && r.Method == http.MethodGet {
			if id := binding.upstreamCursor(cursor); id != "" {
				req.Header.Set("Last-Event-Id", id)
			}
		}
	}
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	headers := proxyHeaders(res.Header)
	if res.StatusCode >= 300 && res.StatusCode < 400 && headers.Get("Location") != "" {
		res.Body.Close()
		return nil, errors.New("upstream redirect refused")
	}
	for _, name := range []string{"Authorization", "Set-Cookie", "Clear-Site-Data", "Refresh"} {
		headers.Del(name)
	}
	// Keep upstream documents from running with the gateway's browser origin.
	headers.Add("Content-Security-Policy", "sandbox; default-src 'none'")
	headers.Set("X-Content-Type-Options", "nosniff")
	headers.Set("Cache-Control", "no-store")
	if p.surface.Auth == "oauth" && headers.Get("WWW-Authenticate") != "" {
		metadata := resourceMetadataURL(p.provider, p.surface)
		switch res.StatusCode {
		case http.StatusUnauthorized:
			headers.Set("WWW-Authenticate", wwwAuthenticate(metadata, "invalid_token"))
		case http.StatusForbidden:
			headers.Set("WWW-Authenticate", wwwAuthenticate(metadata, "insufficient_scope"))
		}
	}
	reply := &proxyReply{status: res.StatusCode, header: headers}
	if reply.status == http.StatusNotFound && binding.id != "" {
		res.Body.Close()
		return reply, nil
	}
	mediaType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" && res.StatusCode == http.StatusOK {
		reply.stream = res.Body
		if binding.id != "" && call.replayable() {
			timer := time.AfterFunc(time.Duration(p.upstream.TimeoutMS)*time.Millisecond, func() { reply.close() })
			err := reply.peekFirstEvent(call)
			timer.Stop()
			if err != nil {
				reply.close()
				return nil, err
			}
		}
		return reply, nil
	}
	defer res.Body.Close()
	timer := time.AfterFunc(time.Duration(p.upstream.TimeoutMS)*time.Millisecond, func() { res.Body.Close() })
	defer timer.Stop()
	reply.body, err = io.ReadAll(io.LimitReader(res.Body, mcp.MaxResponse+1))
	if err != nil || len(reply.body) > mcp.MaxResponse {
		return nil, errors.New("could not read bounded upstream response")
	}
	return reply, nil
}

func proxyHeaders(source http.Header) http.Header {
	out := source.Clone()
	for _, value := range source.Values("Connection") {
		for _, field := range strings.Split(value, ",") {
			out.Del(strings.TrimSpace(field))
		}
	}
	for _, field := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		out.Del(field)
	}
	return out
}

func (r *proxyReply) sessionExpired(call proxyCall) bool {
	if r.expired || r.status == http.StatusNotFound {
		return true
	}
	if (r.status != http.StatusBadRequest && r.status != http.StatusOK) || r.stream != nil {
		return false
	}
	if r.status == http.StatusBadRequest && isInvalidSessionMessage(string(r.body)) {
		return true
	}
	var rpc proxyEnvelope
	return checkUniqueJSONKeys(r.body) == nil && json.Unmarshal(r.body, &rpc) == nil && rpc.JSONRPC == "2.0" &&
		len(rpc.Result) == 0 && rpc.Error != nil && rpc.Error.Code != nil &&
		(bytes.Equal(rpc.ID, []byte("null")) || sameRPCID(rpc.ID, call.ID)) && isInvalidSessionMessage(rpc.Error.Message)
}

func isInvalidSessionMessage(message string) bool {
	switch strings.ToLower(strings.TrimSpace(message)) {
	case "invalid session id", "bad request: invalid session id":
		return true
	default:
		return false
	}
}

func sameRPCID(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if bytes.Equal(a, b) {
		return true
	}
	var left, right string
	return a[0] == '"' && b[0] == '"' && json.Unmarshal(a, &left) == nil && json.Unmarshal(b, &right) == nil && left == right
}

func (r *proxyReply) rpcOutcome(ctx context.Context, call proxyCall, result any) (expired, succeeded bool) {
	if r.sessionExpired(call) {
		return true, false
	}
	if r.status != http.StatusOK {
		return false, false
	}
	matches := func(body []byte) bool {
		var response proxyEnvelope
		return checkUniqueJSONKeys(body) == nil && json.Unmarshal(body, &response) == nil &&
			response.JSONRPC == "2.0" && response.Error == nil && sameRPCID(response.ID, call.ID) &&
			len(response.Result) > 0 && !bytes.Equal(response.Result, []byte("null")) &&
			(result == nil || json.Unmarshal(response.Result, result) == nil)
	}
	if r.stream == nil {
		return false, matches(r.body)
	}
	scanner := sseScanner(io.LimitReader(r.stream, mcp.MaxResponse))
	var data []byte
	for ctx.Err() == nil && scanner.Scan() {
		if eventBoundary(scanner.Bytes()) {
			if len(data) > 0 {
				frame := proxyReply{status: r.status, body: data}
				if frame.sessionExpired(call) {
					return true, false
				}
				if matches(data) {
					return false, true
				}
			}
			data = data[:0]
		} else {
			data = appendEventData(data, scanner.Bytes())
		}
	}
	return false, false
}

// Inspect a read's first complete SSE event before committing a downstream
// response. Preserve the bytes and scanner so ordinary events still stream.
func (r *proxyReply) peekFirstEvent(call proxyCall) error {
	r.lines = sseScanner(r.stream)
	var data []byte
	for r.lines.Scan() {
		line := r.lines.Bytes()
		if len(r.prefix)+len(line) > mcp.MaxResponse {
			return errors.New("upstream SSE event exceeds buffer limit")
		}
		r.prefix = append(r.prefix, line...)
		if eventBoundary(line) {
			frame := proxyReply{status: r.status, body: data}
			r.expired = frame.sessionExpired(call)
			break
		}
		data = appendEventData(data, line)
	}
	return r.lines.Err()
}

func eventBoundary(line []byte) bool {
	return len(bytes.TrimRight(line, "\r\n")) == 0
}

func appendEventData(data, line []byte) []byte {
	value, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
	if ok {
		data = append(data, bytes.TrimPrefix(value, []byte(" "))...)
		data = append(data, '\n')
	}
	return data
}

func (r *proxyReply) close() {
	if r != nil && r.stream != nil {
		r.stream.Close()
	}
}

func (r *proxyReply) write(w http.ResponseWriter, publicID string, binding proxyBinding) {
	r.header.Del(proxySessionHeader)
	if publicID != "" {
		r.header.Set(proxySessionHeader, publicID)
	}
	if r.stream != nil {
		r.header.Del("Content-Length")
		w = &subscriptionDeadlineWriter{ResponseWriter: w}
	}
	for name, values := range r.header {
		w.Header()[name] = values
	}
	w.WriteHeader(r.status)
	if r.stream == nil {
		_, _ = w.Write(r.body)
		return
	}
	controller := http.NewResponseController(w)
	if controller.Flush() != nil {
		return
	}
	writeLine := func(line []byte) bool {
		if binding.generation != 0 && bytes.HasPrefix(line, []byte("id:")) {
			value := bytes.TrimPrefix(line[3:], []byte(" "))
			id := bytes.TrimRight(value, "\r\n")
			if len(id) != 0 && !bytes.ContainsRune(id, 0) {
				line = append([]byte("id: "+binding.cursor(string(id))), value[len(id):]...)
			}
		}
		_, err := w.Write(line)
		return err == nil && controller.Flush() == nil
	}
	prefix := sseScanner(bytes.NewReader(r.prefix))
	for prefix.Scan() {
		if !writeLine(prefix.Bytes()) {
			return
		}
	}
	scanner := r.lines
	if scanner == nil {
		scanner = sseScanner(r.stream)
	}
	for scanner.Scan() {
		if !writeLine(scanner.Bytes()) {
			return
		}
	}
	if scanner.Err() != nil {
		panic(http.ErrAbortHandler)
	}
}

func sseScanner(body io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), mcp.MaxResponse)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			if data[i] == '\r' {
				if i+1 == len(data) && !atEOF {
					return 0, nil, nil
				}
				if i+1 < len(data) && data[i+1] == '\n' {
					i++
				}
			}
			return i + 1, data[:i+1], nil
		}
		if atEOF && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	return scanner
}
