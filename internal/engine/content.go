package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/expr"
	"github.com/burkostya/regate/internal/identity"
	"github.com/burkostya/regate/internal/storage"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const publishedContentTimeout = 90 * time.Second

type contentRunKey struct{}

func effectDiagnostic(ctx context.Context, err error) string {
	if ctx.Value(contentRunKey{}) != nil {
		return "external hook outcome unknown"
	}
	return expr.RedactDiagnostic(err.Error())
}

// ContentMocks supplies wire-shaped synthetic values for management dry runs.
type ContentMocks struct {
	Source map[string]any
	Hooks  map[string]any // By interceptor name, with the hook's native return contract.
}

type publishedContent struct {
	kind, name, uri, source string
	scopes                  []string
	arguments               []config.PromptArgument
	contract                config.ToolContract
	local                   map[string]any
}

func (r *Runtime) ReadPublishedResource(ctx context.Context, cfg *config.Config, revision, surface string, res config.Resource, mocks *ContentMocks) (Outcome, error) {
	item := map[string]any{"uri": res.URI, "text": res.Text}
	if res.MimeType != "" {
		item["mimeType"] = res.MimeType
	}
	target := publishedContent{
		kind: "resource", name: res.Name, uri: res.URI, source: res.Source, scopes: res.RequiredScopes,
		contract: res.Contract, local: map[string]any{"contents": []any{item}},
	}
	return r.runContent(ctx, cfg, revision, surface, target, map[string]any{"uri": res.URI}, mocks)
}

func (r *Runtime) GetPublishedPrompt(ctx context.Context, cfg *config.Config, revision, surface string, prompt config.Prompt, args map[string]any, mocks *ContentMocks) (Outcome, error) {
	target := publishedContent{
		kind: "prompt", name: prompt.Name, source: prompt.Source, scopes: prompt.RequiredScopes,
		arguments: prompt.Arguments, contract: prompt.Contract, local: map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": prompt.Text}},
		}},
	}
	if args == nil {
		args = map[string]any{}
	}
	return r.runContent(ctx, cfg, revision, surface, target, map[string]any{"name": prompt.Name, "arguments": args}, mocks)
}

func (r *Runtime) runContent(ctx context.Context, cfg *config.Config, revision, surface string, target publishedContent, request map[string]any, mocks *ContentMocks) (Outcome, error) {
	ctx, cancel := context.WithTimeout(ctx, publishedContentTimeout)
	defer cancel()
	out := Outcome{RunID: storage.ID(), Revision: revision}
	ctx = withEffectScope(context.WithValue(ctx, contentRunKey{}, true), out.RunID)
	started := time.Now()
	surf := cfg.Surfaces[surface]
	restricted := surf.OAuth != ""
	interceptors := cfg.ContentInterceptors(surface, target.kind, target.name)
	upstreamName, remote, validSource := strings.Cut(target.source, ".")
	upstream := cfg.Upstreams[upstreamName]
	// Check the complete destination set before a hook can receive caller data.
	// Authorization and trust failures deliberately precede on_error dispatch.
	if mocks == nil {
		for _, scopes := range [][]string{surf.RequiredScopes, target.scopes} {
			if err := identity.RequireScopes(ctx, scopes); err != nil {
				out.Status = "denied"
				return out, err
			}
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
	}
	if target.source != "" && (!validSource || upstream.URL == "") {
		out.Status = "failure"
		return out, errors.New("invalid content source")
	}
	if restricted {
		if target.source != "" {
			if err := requireRestrictedDestination("upstream "+upstream.Name, upstream.TrustLevel); err != nil {
				out.Status = "denied"
				return out, ErrUntrustedDestination
			}
		}
		for _, i := range interceptors {
			h := contentHook(cfg, i)
			if h.Transport == "" {
				continue
			}
			if err := requireRestrictedHook(cfg, h); err != nil {
				out.Status = "denied"
				return out, ErrUntrustedDestination
			}
		}
	}

	status := "success"
	result, err := r.executeContent(ctx, cfg, target, request, upstream, remote, interceptors, restricted, mocks)
	if err != nil {
		status = "failure"
		message := "operation failed"
		if errors.Is(err, ErrDenied) {
			status, message = "denied", "operation denied"
		}
		if mocks != nil {
			// Draft checks see stage names and synthetic fixture errors.
			out.Error = expr.RedactDiagnostic(err.Error())
		} else {
			out.Error = message
		}
		_, _ = r.interceptContent(ctx, cfg, interceptors, "on_error", map[string]any{
			"status": status, "message": message, "run_id": out.RunID,
		}, nil, mocks, nil)
		err = &jsonrpc.Error{Code: -32000, Message: out.Error}
	}
	out.Status = status
	if err == nil {
		out.Value = result
	}
	if mocks == nil && r.Store != nil {
		r.Store.WriteTrace(storage.Trace{RunID: out.RunID, Revision: revision, Surface: surface,
			Tool: target.kind + ":" + target.name, Status: status, Message: out.Error, DurationMS: time.Since(started).Milliseconds()})
	}
	return out, err
}

func (r *Runtime) executeContent(ctx context.Context, cfg *config.Config, target publishedContent, request map[string]any, upstream config.Upstream, remote string, interceptors []config.Interceptor, restricted bool, mocks *ContentMocks) (map[string]any, error) {
	if err := target.validateRequest(request); err != nil {
		return nil, err
	}
	validateRequest := func(value map[string]any) error { return target.validateRequest(value) }
	request, err := r.interceptContent(ctx, cfg, interceptors, "on_request", request, request, mocks, validateRequest)
	if err != nil {
		return nil, err
	}

	validateResult := func(value map[string]any) error {
		if err := validateContentResult(target.kind, value); err != nil {
			return err
		}
		return target.contract.Output.Validate(value)
	}
	result := target.local
	if target.source != "" {
		request, err = r.interceptContent(ctx, cfg, interceptors, "before_call", request, request, mocks, validateRequest)
		if err != nil {
			return nil, err
		}
		if mocks != nil {
			result = cloneContent(mocks.Source)
			if result == nil {
				return nil, errors.New("source mock required for an upstream publication")
			}
		} else {
			result, err = r.callContentSource(ctx, cfg, upstream, target, remote, request)
			if err != nil {
				return nil, err
			}
		}
		if err := validateContentResult(target.kind, result); err != nil {
			return nil, err
		}
		result, err = r.interceptContent(ctx, cfg, interceptors, "after_call", result, request, mocks,
			func(value map[string]any) error { return validateContentResult(target.kind, value) })
		if err != nil {
			return nil, err
		}
	}
	if err := validateResult(result); err != nil {
		return nil, err
	}
	result, err = r.interceptContent(ctx, cfg, interceptors, "before_response", result, request, mocks, validateResult)
	if err != nil {
		return nil, err
	}
	if restricted {
		if err := maskContentResult(target.kind, result); err != nil {
			return nil, err
		}
	}
	if target.kind == "resource" && (restricted || len(interceptors) != 0) {
		result["ttlMs"], result["cacheScope"] = 0, "private"
	}
	return result, validateResult(result)
}

func (p publishedContent) validateRequest(value map[string]any) error {
	if p.kind == "resource" {
		if len(value) != 1 || value["uri"] != p.uri {
			return errors.New("resource input must retain its published URI")
		}
	} else {
		args, ok := value["arguments"].(map[string]any)
		if len(value) != 2 || value["name"] != p.name || !ok || args == nil {
			return errors.New("prompt input must retain its published name and an arguments object")
		}
		for _, value := range args {
			if _, ok := value.(string); !ok {
				return errors.New("prompt arguments must be strings")
			}
		}
		for _, argument := range p.arguments {
			if _, present := args[argument.Name]; argument.Required && !present {
				return fmt.Errorf("missing prompt argument %s", argument.Name)
			}
		}
	}
	return p.contract.Input.Validate(value)
}

func (r *Runtime) callContentSource(ctx context.Context, cfg *config.Config, upstream config.Upstream, target publishedContent, remote string, request map[string]any) (map[string]any, error) {
	if err := checkOutbound(upstream.URL); err != nil {
		return nil, err
	}
	conn, err := r.connFor(ctx, cfg, upstream)
	if err != nil {
		return nil, err
	}
	method, params := "resources/read", request
	if target.kind == "prompt" {
		method = "prompts/get"
		params = map[string]any{"name": remote, "arguments": request["arguments"]}
	}
	data, err := conn.Call(ctx, method, params)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if target.kind == "resource" {
		normalizeEmptyResourceText(result["contents"])
	} else if messages, ok := result["messages"].([]any); ok {
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			content, _ := message["content"].(map[string]any)
			if content["type"] == "resource" {
				normalizeEmptyResourceText([]any{content["resource"]})
			}
		}
	}
	return result, nil
}

func contentHook(cfg *config.Config, i config.Interceptor) config.Hook {
	if i.Policy != "" {
		return cfg.Hooks[cfg.Policies[i.Policy].DecisionHook]
	}
	return cfg.Hooks[i.Hook]
}

func (r *Runtime) interceptContent(ctx context.Context, cfg *config.Config, interceptors []config.Interceptor, stage string, value, request map[string]any, mocks *ContentMocks, validate func(map[string]any) error) (map[string]any, error) {
	for _, i := range interceptors {
		if i.Stage != stage {
			continue
		}
		varsRequest := request
		if stage == "on_request" || stage == "before_call" {
			varsRequest = value
		}
		next, err := r.contentStep(withEffectStep(ctx, "interceptor:"+stage+":"+i.Name), cfg, i, value, varsRequest, mocks)
		if stage == "on_error" {
			continue // Every observer is independent and receives the same generic diagnostic.
		}
		if err != nil {
			return nil, fmt.Errorf("%s interceptor %s: %w", stage, i.Name, err)
		}
		value, _ = next.(map[string]any)
		if value == nil {
			return nil, fmt.Errorf("%s interceptor %s must produce an object", stage, i.Name)
		}
		if err := validate(value); err != nil {
			return nil, fmt.Errorf("%s interceptor %s: %w", stage, i.Name, err)
		}
	}
	return value, nil
}

func (r *Runtime) contentStep(ctx context.Context, cfg *config.Config, i config.Interceptor, value, request map[string]any, mocks *ContentMocks) (any, error) {
	var input any = value
	if i.Program != nil {
		var err error
		input, err = i.Program.Eval(ctx, map[string]any{"input": value, "request": request})
		if err != nil {
			return nil, err
		}
	}
	if i.Check != "" {
		if input != true {
			return nil, ErrDenied
		}
		return value, nil
	}
	if i.Transform != "" {
		return input, nil
	}
	h := contentHook(cfg, i)
	var output any
	var err error
	if mocks != nil && h.Transport != "builtin" {
		if i.Stage == "on_error" {
			return nil, nil
		}
		var found bool
		output, found = mocks.Hooks[i.Name]
		if !found {
			return nil, errors.New("external hook mock required")
		}
		output, err = hookResponseValue(h, output)
	} else {
		output, err = r.runHook(ctx, h, cfg, input, false)
	}
	if err != nil {
		return nil, err
	}
	if h.Contract == "decision" || h.Contract == "observe" {
		return value, nil
	}
	if mocks != nil && h.Transport != "builtin" {
		// Treat each fixture as an independent response, even if reused by another dry run.
		if mapped, ok := output.(map[string]any); ok {
			return cloneContent(mapped), nil
		}
	}
	return output, nil
}

func cloneContent(value map[string]any) map[string]any {
	var out map[string]any
	raw, _ := json.Marshal(value)
	_ = json.Unmarshal(raw, &out)
	return out
}

func validateContentResult(kind string, result map[string]any) error {
	if t, present := result["resultType"]; present && t != "complete" {
		return errors.New("content result must be complete")
	}
	if result["requestState"] != nil || result["inputRequests"] != nil {
		return errors.New("content result cannot require another request")
	}
	if kind == "resource" {
		items, ok := result["contents"].([]any)
		if !ok || items == nil {
			return errors.New("resource result requires a contents array")
		}
		for _, item := range items {
			if err := validateResourceItem(item); err != nil {
				return err
			}
		}
		var wire sdk.ReadResourceResult
		return decodeContentResult(result, &wire)
	}
	items, ok := result["messages"].([]any)
	if !ok || items == nil {
		return errors.New("prompt result requires a messages array")
	}
	for _, item := range items {
		message, _ := item.(map[string]any)
		if message["role"] != "user" && message["role"] != "assistant" {
			return errors.New("prompt message requires a user or assistant role")
		}
		content, _ := message["content"].(map[string]any)
		switch content["type"] {
		case "text":
			if _, ok := content["text"].(string); !ok {
				return errors.New("prompt text content requires text")
			}
		case "resource":
			if err := validateResourceItem(content["resource"]); err != nil {
				return err
			}
		case "resource_link":
			if !validContentURI(content["uri"]) {
				return errors.New("prompt resource link requires an absolute URI")
			}
			if name, _ := content["name"].(string); name == "" {
				return errors.New("prompt resource link requires a name")
			}
		case "image", "audio":
			mime, _ := content["mimeType"].(string)
			if _, ok := content["data"].(string); !ok || mime == "" {
				return errors.New("prompt media content requires data and a MIME type")
			}
		default:
			return errors.New("invalid prompt content type")
		}
	}
	var wire sdk.GetPromptResult
	return decodeContentResult(result, &wire)
}

func validateResourceItem(value any) error {
	item, _ := value.(map[string]any)
	if !validContentURI(item["uri"]) {
		return errors.New("resource content requires an absolute URI")
	}
	if _, blob := item["blob"]; blob {
		if _, text := item["text"]; text {
			return errors.New("resource content cannot contain both text and blob")
		}
		if _, ok := item["blob"].(string); !ok {
			return errors.New("resource blob must be a base64 string")
		}
	}
	if text, present := item["text"]; present {
		if _, ok := text.(string); !ok {
			return errors.New("resource text must be a string")
		}
	} else if _, blob := item["blob"]; !blob {
		return errors.New("resource content requires text or blob")
	}
	return nil
}

// The SDK omits empty text when re-encoding upstream resource contents.
func normalizeEmptyResourceText(raw any) {
	items, _ := raw.([]any)
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		_, text := item["text"]
		_, blob := item["blob"]
		if !text && !blob {
			item["text"] = ""
		}
	}
}

func validContentURI(value any) bool {
	text, _ := value.(string)
	parsed, err := url.Parse(text)
	return err == nil && parsed.IsAbs()
}

func decodeContentResult(result map[string]any, wire any) error {
	raw, err := json.Marshal(result)
	if err != nil || len(raw) > maxHTTPHookRequestBytes {
		return errors.New("content result exceeds the JSON limit or cannot be encoded")
	}
	if err := json.Unmarshal(raw, wire); err != nil {
		return errors.New("invalid MCP content result")
	}
	return nil
}

func maskContentResult(kind string, result map[string]any) error {
	if kind == "resource" {
		for _, raw := range result["contents"].([]any) {
			if _, opaque := raw.(map[string]any)["blob"]; opaque {
				return errors.New("opaque binary resource content is unavailable over OAuth")
			}
		}
	} else {
		for _, raw := range result["messages"].([]any) {
			content := raw.(map[string]any)["content"].(map[string]any)
			switch content["type"] {
			case "image", "audio":
				return errors.New("opaque prompt media is unavailable over OAuth")
			case "resource":
				if _, opaque := content["resource"].(map[string]any)["blob"]; opaque {
					return errors.New("opaque embedded resource content is unavailable over OAuth")
				}
			}
		}
	}
	masked := expr.MaskPII(result).(map[string]any)
	clear(result)
	for key, value := range masked {
		result[key] = value
	}
	return nil
}
