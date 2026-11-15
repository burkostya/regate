# Security and operational boundaries

**This implementation is a prototype for local testing. It has not been security audited and must not be Internet-exposed or used with sensitive personal/production data.**

## What is enforced now

- Admin bearer token is mandatory for CLI start and protected management endpoints.
- Public token is mandatory for non-loopback binding; anonymous public surface calls are loopback-only by default.
- Constant-time comparison for bearer equality.
- No upstream token is passed through automatically; authentication is looked up from a named environment variable (`auth_env`).
- HTTP hook and MCP upstream outbound host allowlist, no HTTP redirects, HTTP(S) URL validation.
- HCL changes pass validation and draft base revision conflicts are rejected.
- Decision `deny`/`indeterminate`/network errors and unfulfilled obligations fail closed.
- The expression evaluator accepts a finite safe subset without arbitrary code execution.
- Suspended calls and task statuses have file-backed persistence; event journal records dispatching/unknown states.

## Missing before production deployment

1. Use the official `modelcontextprotocol/go-sdk` with conformance tests, complete multi-version negotiation, stream cancellation, subscription closing and OAuth protected resources.
2. Authenticate principals with scoped credentials/issuer+audience validation; implement per-surface and per-tool authorization rather than a shared public bearer only.
3. Prevent DNS rebinding and private-network SSRF at the resolver/dialer layer. Enforce outgoing request size, data volume and content policies.
4. Complete trusted PII detection, classification, labels, provenance, declassification proofs, consent/retention and no-leak invariants.
5. Implement EffectContract verification, remote idempotency support, read-back reconciliation and compensation mechanisms for unclear write results.
6. Add complete HCL2+CEL typed validation, JSON Schema validation, compilation against the observed upstream schema and semantic impact analysis.
7. Strengthen the persistent state with transactional database semantics, file encryption/access controls, key rotation, safe migrations and leader election if multi-process.
8. Audit every hook, identity, policy decision and changed configuration, with reliable tamper-aware records and redacted observability.
9. Establish rate limiting, concurrency fairness, resource budgets, body and SSE limits, task quotas and robust shutdown.
10. Implement sandboxing/attestation of executable adapters, dependency pinning, SBOM, fuzz/property testing and a security review.

**Known demo-only unsafe tool:** `get_customer_direct` intentionally publishes an unmasked CRM result. It exists to test transparent MCP proxying. Do not distribute that surface without explicit access controls.

## Configuration secrets

Use `REGATE_ADMIN_TOKEN` for management, `REGATE_PUBLIC_TOKEN` for non-loopback, `REGATE_ALLOWED_HOSTS` for approved HTTPS upstreams, `REGATE_SKILLS_DIR` for installed skill docs. Upstream/hook `auth_env` fields contain names of environment variables, not credentials. Never place bearer tokens in HCL source or UI layout.
