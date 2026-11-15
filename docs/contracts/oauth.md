# OAuth resource-server contract

Regate does **not** issue tokens. Every protected MCP surface is an OAuth
resource server. A trusted external authorization server issues access tokens;
Regate verifies them before exposing a catalog, tool, resource, prompt or task.

```hcl
oauth "company" {
  issuer     = "https://idp.example.com/"
  audience   = "https://regate.example.com/mcp/support"
  jwks_url   = "https://idp.example.com/.well-known/jwks.json"

  # Optional RFC 7662 path for opaque access tokens:
  # introspection_url = "https://idp.example.com/oauth2/introspect"
  # client_id_env     = "COMPANY_INTROSPECTION_CLIENT_ID"
  # client_secret_env = "COMPANY_INTROSPECTION_CLIENT_SECRET"
}

surface "support" {
  path            = "/mcp/support"
  oauth           = "company"
  required_scopes = ["regate:access"]

  tool "customer_read" {
    source             = "crm.get_customer"
    description        = "Scoped CRM customer lookup"
    required_scopes    = ["crm:read"]
    data_classification = "restricted"
    read_only          = true
  }
}
```

The actual tool/upstream/pipeline names and references must also exist in the
same HCL file. The example is an authorization fragment.

## Fail-closed verification

- Exactly one of the existing OAuth providers must be named by a protected
  surface. Anonymous loopback and `REGATE_PUBLIC_TOKEN` do **not** bypass it.
- Signed JWT validation supports pinned JWKS keys for RS256, PS256 and ES256,
  rejects `alg=none`, unsupported key types, duplicate claims and wrong
  signatures. RSA keys must have at least 2048 bits. The JOSE `kid` must match
  one verified key and every JWT must have an exact issuer, resource audience,
  subject and unexpired `exp`; `iat`/`nbf` are enforced when present.
- Opaque tokens can be verified through an explicitly configured RFC 7662
  introspection endpoint. The `active` bit is required and the same issuer,
  audience, subject and expiry checks apply. Introspection credentials are
  loaded from **environment variable names**; they are never stored in HCL.
- All JWKS/introspection HTTP requests use the guarded outbound client, deny
  redirects and apply DNS/IP egress checks. External HTTPS hostnames must be
  explicitly included in `REGATE_ALLOWED_HOSTS`. Loopback is for testing.
- Per-surface scopes are required for every request; per-tool, per-resource and
  per-prompt scopes are also enforced on calls and filtered from discovery.
  Authentication does not imply unrestricted catalog enumeration.
- The management REST/MCP endpoints retain their separate
  `REGATE_ADMIN_TOKEN` requirement. That token does not authenticate to
  OAuth-protected public surfaces, and OAuth access tokens never authorize
  management operations.
- Resource discovery is published at
  `/.well-known/oauth-protected-resource` plus the MCP surface path, with
  RFC 9728 `resource`, `authorization_servers` and `scopes_supported`.
  Rejected OAuth requests provide an appropriate Bearer challenge.
- Inbound access tokens are **not** forwarded to upstream MCP or HTTP
  endpoints; outbound credentials come only from explicitly configured
  `auth_env` values. This prevents wrong-audience token pass-through.

## Sensitive results and async tasks

With OAuth enabled, tool content and structured results are recursively
PII/credential-masked by default. A tool may opt into an unmasked release
**only** with all three declarations: `data_classification="restricted"`,
`allow_unmasked=true`, and `required_scopes` containing `pii:read`.
Such an explicit release remains an operator-controlled declassification
decision; it is not a claim that the destination is trustworthy.

Resource and prompt text is masked by default. Opaque upstream binary
resources are denied when the gateway cannot establish how to redact them.
Diagnostic errors are generic for OAuth-protected invocations, and traces
never persist raw OAuth operation error strings. Raw tokens and credentials
must never be included in log fields or configuration values.

Async tasks retain their creation-time tool scopes and masking obligation.
After token renewal, task access is bound to the verified issuer, audience and
subject rather than the exact bearer bytes. Calls to get, update and cancel
re-check both the original tool scopes and the originating MCP surface.
The unfinished durable token-exchange/continuation contract remains separate.

## What is not proven by this milestone

The current PII classifier is not a formal proof of full declassification:
names, arbitrary identifiers, multimedia and other sensitive content can still
require dedicated DLP policies/verified transforms. End-to-end taint tracking,
delegated OAuth token exchange with target-audience validation, revocation
propagation, live validation against each external IdP, strong audit
attestations and production threat review remain release gates. Do not use
these tests to claim general-purpose safe handling of arbitrary personal
information or a complete v1 release.
