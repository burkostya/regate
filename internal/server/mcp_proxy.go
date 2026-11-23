package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/burkostya/regate/internal/config"
	"github.com/burkostya/regate/internal/identity"
	"github.com/burkostya/regate/internal/netguard"
)

const proxySessionHeader = "Mcp-Session-Id"

type publishedMCP struct {
	surface  config.Surface
	upstream config.Upstream
	provider config.OAuth
	exchange config.OAuth
	target   *url.URL
	client   *http.Client
	sessions *proxySessions
}

func newPublishedMCP(cfg *config.Config, surface config.Surface) (*publishedMCP, error) {
	u := cfg.Upstreams[surface.Upstream]
	target, err := url.Parse(u.URL)
	if err != nil {
		return nil, errors.New("invalid published upstream URL")
	}
	return &publishedMCP{
		surface: surface, upstream: u, provider: cfg.OAuth[surface.OAuth], exchange: cfg.OAuth[u.OAuthExchange],
		target: target, client: netguard.StreamingClient(time.Duration(u.TimeoutMS) * time.Millisecond),
		sessions: new(proxySessions),
	}, nil
}

func (p *publishedMCP) sameRoute(old *publishedMCP) bool {
	return p.surface.Name == old.surface.Name && p.surface.Auth == old.surface.Auth &&
		p.surface.BearerTokenEnv == old.surface.BearerTokenEnv && slices.Equal(p.surface.RequiredScopes, old.surface.RequiredScopes) &&
		p.provider == old.provider && p.exchange == old.exchange && p.upstream.Name == old.upstream.Name &&
		p.upstream.URL == old.upstream.URL && p.upstream.AuthEnv == old.upstream.AuthEnv &&
		p.upstream.OAuthAudience == old.upstream.OAuthAudience && slices.Equal(p.upstream.OAuthScopes, old.upstream.OAuthScopes)
}

func (p *publishedMCP) authorize(w http.ResponseWriter, r *http.Request) bool {
	switch p.surface.Auth {
	case "anonymous":
		return true
	case "bearer":
		token := bearer(r)
		if len(r.Header.Values("Authorization")) == 1 && !constantTimeEq(token, AdminToken()) &&
			constantTimeEq(token, os.Getenv(p.surface.BearerTokenEnv)) {
			return true
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

func (p *publishedMCP) serve(s *Service, w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions:
	default:
		w.Header().Set("Allow", "GET, POST, DELETE, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodPost && r.ContentLength != 0 {
		http.Error(w, "MCP request body requires POST", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxSubscriptionLifetime)
	defer cancel()
	r = r.WithContext(ctx)
	if err := s.Store.RequireExecutionLeader(ctx); err != nil {
		http.Error(w, "execution unavailable", http.StatusServiceUnavailable)
		return
	}

	call, err := readProxyCall(r)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "MCP request exceeds 4 MiB", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid MCP request", http.StatusBadRequest)
		}
		return
	}
	if r.Method == http.MethodGet || call.Method == "subscriptions/listen" {
		if !s.subscriptions.acquire() {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "subscription capacity reached", http.StatusServiceUnavailable)
			return
		}
		defer s.subscriptions.release()
	}

	owner := proxyOwner{query: sha256.Sum256([]byte(r.URL.RawQuery))}
	var token string
	switch p.surface.Auth {
	case "bearer":
		token = os.Getenv(p.upstream.AuthEnv)
		if token == "" || constantTimeEq(token, AdminToken()) {
			http.Error(w, "upstream credential unavailable", http.StatusBadGateway)
			return
		}
		owner.inbound = sha256.Sum256([]byte(bearer(r)))
		owner.outbound = sha256.Sum256([]byte(token))
	case "oauth":
		principal, ok := identity.FromContext(ctx)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		owner.issuer, owner.audience, owner.subject = principal.Issuer, principal.Audience, principal.Subject
	}

	publicID := r.Header.Get(proxySessionHeader)
	var session *proxySession
	if publicID != "" {
		session = p.sessions.find(publicID, owner)
		if session == nil {
			http.Error(w, "MCP session not found", http.StatusNotFound)
			return
		}
	}
	if p.surface.Auth == "oauth" {
		token, err = identity.Exchange(ctx, p.exchange, p.upstream.OAuthAudience, p.upstream.OAuthScopes)
		if err != nil {
			http.Error(w, "upstream authorization unavailable", http.StatusBadGateway)
			return
		}
	}

	if session != nil {
		p.serveSession(w, r, token, call, publicID, session)
		return
	}
	res, err := p.send(r, token, call, proxyBinding{})
	if err != nil {
		mayWrite := r.Method == http.MethodPost && !call.replayable() && !call.request("initialize") &&
			!call.isMethod("notifications/initialized")
		proxyFailure(w, mayWrite)
		return
	}
	defer res.close()

	var binding proxyBinding
	if upstreamID := res.header.Get(proxySessionHeader); upstreamID != "" {
		res.header.Del(proxySessionHeader)
		if call.request("initialize") && res.status == http.StatusOK {
			if !validProxySessionID(upstreamID) {
				proxyFailure(w, false)
				return
			}
			var ok bool
			publicID, binding, ok = p.sessions.add(owner, upstreamID, call.body, r.Header.Get("Mcp-Protocol-Version"))
			if !ok {
				http.Error(w, "MCP session capacity reached", http.StatusServiceUnavailable)
				return
			}
		}
	}
	res.write(w, publicID, binding)
}

func (p *publishedMCP) serveSession(w http.ResponseWriter, r *http.Request, token string, call proxyCall, publicID string, session *proxySession) {
	ctx := r.Context()
	if call.request("initialize") {
		http.Error(w, "initialize requires a new MCP session", http.StatusBadRequest)
		return
	}
	initialized := call.isMethod("notifications/initialized") && len(call.ID) == 0
	binding := session.current()
	recoverSession := func() bool {
		var ok bool
		binding, ok = session.recover(ctx, binding, func() (string, bool) {
			return p.initializeSession(r, token, session)
		})
		return ok
	}
	if binding.stale && (r.Method == http.MethodDelete || call.callback()) {
		if r.Method == http.MethodDelete {
			p.sessions.remove(publicID, session)
		}
		http.Error(w, "MCP session not found", http.StatusNotFound)
		return
	}
	if binding.stale {
		if !recoverSession() {
			proxyFailure(w, false)
			return
		}
		if initialized {
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	// Check state before an operation that could write. A callback answers an
	// upstream request, so pinging first can block the operation awaiting it.
	mayWrite := r.Method == http.MethodPost && !call.replayable() && !initialized && !call.callback()
	if mayWrite {
		expired, ok := p.pingSession(r, token, binding)
		if !ok || (expired && !recoverSession()) {
			proxyFailure(w, false)
			return
		}
	}
	res, err := p.send(r, token, call, binding)
	if err != nil {
		proxyFailure(w, mayWrite)
		return
	}
	defer func() { res.close() }()
	if res.sessionExpired(call) {
		session.invalidate(binding)
		switch {
		case r.Method == http.MethodDelete:
			p.sessions.remove(publicID, session)
			http.Error(w, "MCP session not found", http.StatusNotFound)
			return
		case r.Method == http.MethodGet || call.replayable() || initialized:
			res.close()
			if !recoverSession() {
				proxyFailure(w, false)
				return
			}
			if initialized {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			res, err = p.send(r, token, call, binding)
			if err != nil {
				res = nil
				proxyFailure(w, false)
				return
			}
			if res.sessionExpired(call) {
				session.invalidate(binding)
				proxyFailure(w, false)
				return
			}
		default:
			proxyFailure(w, mayWrite)
			return
		}
	}
	if r.Method == http.MethodDelete && res.status >= 200 && res.status < 300 {
		p.sessions.remove(publicID, session)
	}
	if res.header.Get(proxySessionHeader) == "" {
		publicID = ""
	}
	res.write(w, publicID, binding)
}

func (p *publishedMCP) pingSession(r *http.Request, token string, binding proxyBinding) (expired, ok bool) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(p.upstream.TimeoutMS)*time.Millisecond)
	defer cancel()
	ping := internalProxyCall("ping", true)
	res, err := p.send(r.WithContext(ctx), token, ping, binding)
	if err != nil {
		return false, false
	}
	defer res.close()
	expired, succeeded := res.rpcOutcome(ctx, ping, nil)
	return expired, expired || succeeded
}

func (p *publishedMCP) initializeSession(r *http.Request, token string, session *proxySession) (string, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(p.upstream.TimeoutMS)*time.Millisecond)
	defer cancel()
	r = r.WithContext(ctx)
	call := internalProxyCall("initialize", false)
	call.body, call.version = session.initialize, session.initializeVersion
	if json.Unmarshal(call.body, &call.proxyEnvelope) != nil {
		return "", false
	}
	res, err := p.send(r, token, call, proxyBinding{})
	if err != nil {
		return "", false
	}
	defer res.close()
	id := res.header.Get(proxySessionHeader)
	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if !validProxySessionID(id) {
		return "", false
	}
	if _, succeeded := res.rpcOutcome(r.Context(), call, &result); !succeeded || result.ProtocolVersion == "" {
		return "", false
	}
	if expected := r.Header.Get("Mcp-Protocol-Version"); expected != "" && expected != result.ProtocolVersion {
		return "", false
	}
	initialized := internalProxyCall("notifications/initialized", false)
	initialized.version = result.ProtocolVersion
	ack, err := p.send(r, token, initialized, proxyBinding{id: id})
	if err != nil {
		return "", false
	}
	defer ack.close()
	return id, ack.status == http.StatusAccepted
}

func validProxySessionID(id string) bool {
	return id != "" && len(id) <= 1024 && strings.IndexFunc(id, func(r rune) bool { return r < 0x21 || r > 0x7e }) < 0
}

func proxyFailure(w http.ResponseWriter, unknown bool) {
	if unknown {
		http.Error(w, "upstream operation outcome is unknown", http.StatusBadGateway)
	} else {
		http.Error(w, "upstream MCP unavailable", http.StatusBadGateway)
	}
}
