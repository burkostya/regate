package config

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

func (c *Config) validatePublishedUpstream(surface Surface) error {
	fail := func(message string) error {
		return fmt.Errorf("surface %s: %s", surface.Name, message)
	}
	u, exists := c.Upstreams[surface.Upstream]
	if !exists {
		return fail("undefined upstream")
	}
	if len(surface.Tools)+len(surface.Resources)+len(surface.Prompts) != 0 {
		return fail("upstream publication cannot include local tools, resources or prompts")
	}
	p := surface.Path
	if strings.HasPrefix(p, "/mcp/admin/") {
		return fail("published upstream overlaps reserved path")
	}
	if !strings.HasPrefix(p, "/mcp/") || path.Clean(p) != p || (&url.URL{Path: p}).EscapedPath() != p {
		return fail("published upstream path must be a canonical path under /mcp/")
	}
	if surface.Auth != "bearer" && surface.BearerTokenEnv != "" {
		return fail("bearer_token_env requires bearer authorization")
	}
	if surface.Auth != "oauth" && surface.OAuth != "" {
		return fail("oauth provider requires oauth authorization")
	}
	switch surface.Auth {
	case "anonymous":
		if u.AuthEnv != "" || u.OAuthExchange != "" {
			return fail("anonymous publication requires an upstream without configured credentials")
		}
	case "bearer":
		if !safeEnvName(surface.BearerTokenEnv) || surface.BearerTokenEnv == "REGATE_ADMIN_TOKEN" ||
			u.AuthEnv == "" || u.AuthEnv == "REGATE_ADMIN_TOKEN" || surface.BearerTokenEnv == u.AuthEnv {
			return fail("bearer publication requires distinct public bearer_token_env and upstream auth_env variables")
		}
	case "oauth":
		provider, ok := c.OAuth[surface.OAuth]
		exchange, delegated := c.OAuth[u.OAuthExchange]
		audience, err := url.Parse(provider.Audience)
		if !ok || !delegated || provider.Issuer != exchange.Issuer || u.OAuthAudience == provider.Audience ||
			err != nil || audience.Host == "" || (audience.Scheme != "https" && audience.Scheme != "http") ||
			audience.EscapedPath() != p || audience.User != nil || audience.ForceQuery || audience.RawQuery != "" || audience.Fragment != "" {
			return fail("oauth publication requires a surface audience URL and a same-issuer exchange for a distinct upstream audience")
		}
	default:
		return fail("upstream publication requires auth = anonymous, bearer, or oauth")
	}
	return nil
}
