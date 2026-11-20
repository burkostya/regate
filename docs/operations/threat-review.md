# Operational threat review

The local engineering review for #9 covers alpha and beta, independent
read-only providers, and the controlled Keycloak and NGINX profiles.
Alpha and beta cover v1 durable-write acceptance.
[Acceptance decision](https://github.com/burkostya/regate/issues/106)

| Threat / abuse case | Implemented defense | Repeatable evidence | Residual risk |
|---|---|---|---|
| Remote actor saturates public MCP | Fixed in-flight public slots, bounded body, per-peer socket-IP token bucket and optional shared OAuth principal quota. Reject before effects. | `TestHTTPAdmissionDoesNotStarveAdminOrBreakStreaming`, `TestRateAdmissionNeverDispatchesDeniedPublicCallsOrConsumesAdminQuota`, `TestV1Soak` | Multi-host DoS protection depends on the target ingress and deployment |
| Remote actor spoofs forwarding headers | Socket peer is derived from the connection. The optional shared quota applies to authenticated OAuth issuer and subject pairs. | `TestPeerQuotaSameIPDifferentPortsAndIndependentPeers`, `TestV1TenantBoundaries` | Unauthenticated callers behind a proxy share its socket-peer quota |
| Many source IPs exhaust limiter metadata | Fixed max 1024 peers; reject new identities when full and clean stale entries | `TestRateQuotasBoundTrackingAndRejectMalformedConfiguration` | Legitimate new clients may receive 429 under adversarial exhaustion |
| MCP request abuse or HTTP chunked request overflow | Hard 4 MiB request limit plus strict JSON frame/duplicate key validation; 2 MiB admin object cap | `TestHTTPBodyCeilingWithoutEffectDispatch`, `FuzzStrictAdminJSONObjectFraming` | SSE transport flood and upstream response limits require further multi-tenant testing |
| Operational metrics exfiltrate PII | Only bounded category labels/status buckets/aggregate time; no raw path/query/header, inputs, MCP methods or actor | `TestHTTPMetricsPrivateAndFixedCardinality`, `TestOTLPTraceExportContainsOnlyBoundedLabels` | External collectors need controlled retention and TLS/host allowlisting |
| Telemetry exporter outage blocks effect safety | Async bounded 256-span queue; 32-span batches, 2-second timeout, drop counter; egress DNS/redirect/proxy protections | `TestCollectorQueueOutageIsBoundedAndCountersRemainPrivate`, OTLP integration test | Spans may be dropped; telemetry not an authoritative effect journal |
| Local audit record modification or deletion | SQLite append-only-by-API audit chain, before-mutation barrier and offline backup verification against a signed checkpoint | `TestAuditChainRestartParallelAndTamperRefusal`, `make test-v1-controlled`, `make test-v1-audit-retention` | Independent signer custody and off-host immutable retention require deployment and review |
| Arbitrary credentials/data leak through management logs | Audit taxonomy allowlist (category/action/outcome), never accepts free-text/payload; admin bearer protected | `TestManagementMutationsCreateRedactedAuditedBeforeAndAfterRecords` | Full label-aware PII taint proof and signed actor identity still need external review |
| Uncertain external write blindly retried after timeout/lost response | Durable journal, contract-bound positive readback, provider-native idempotency/fence/tombstone in disposable alpha/beta OS processes | `TestTwoProcessMCPDurableProviderContracts`, `TestTwoProcessMCPReadLoadIsolation` | Guarantees for other providers depend on their contracts |
| Decision deny races a write | Mandatory policy compiler dominance and runtime check before effects | DAG/TLC checks and the RC refusal tests below | Review the organization's decision rules and data-processing obligations |

## Operator checks

Run `go test -race ./...`, `go vet ./...`, `go build ./cmd/regate ./cmd/regate-demo`, `go test -race ./internal/server -run TestTwoProcessMCP -count=1 -v`. Scheduled GitHub Actions also runs `TestTwoProcessMCPReadLoadIsolation` and an explicit 20-second JSON framing fuzz campaign; Go normal tests run fuzz seeds. Set distinct secrets in the environment and never put production data in these tests.

Quota environment variables: `REGATE_PUBLIC_RPS` (default 300), `REGATE_PUBLIC_BURST` (600), `REGATE_ADMIN_RPS` (100), `REGATE_ADMIN_BURST` (200), each 1..10000. These socket-peer limits are per process. Both limiter maps are bounded to 1024 ephemeral peer digests. The optional SQLite-backed OAuth budget survives same-host process promotion. A saturated limiter returns HTTP 429 before a tool can dispatch. [Quota settings](telemetry.md#http-admission-telemetry-and-privacy-boundaries)

## v1.0.0-rc.1 security and PII review

The [local acceptance report](https://github.com/burkostya/regate/releases/download/v1.0.0-rc.1/regate-v1-rc-892b6d3-acceptance.json)
covers the unpacked Linux x86-64 gateway and console from `892b6d3`. The stand
uses synthetic records, separate alpha and beta processes, two NGINX TLS
ingresses and two Keycloak realms. The tenant boundary test uses fixture
JWTs with deliberately identical subjects across issuers and overlapping
record IDs. See the [controlled topology](../testing/two-mcp-stand.md#controlled-local-topology).

| Boundary | Evidence from the RC and regression suite | Review for sign-off |
|---|---|---|
| Tenant identity and task ownership | The tenant test rejected wrong-issuer requests and foreign task lookup, update and cancel. Each tenant's write went to its own provider. OAuth tests verify audience, scope-filtered discovery and separate management authorization. [Tenant scenario](../../internal/server/v1_tenant_test.go) · [OAuth tests](../../internal/server/oauth_integration_test.go) | Review the actual issuer, audience, scopes, tenant-to-surface routing and privileged access policy. |
| Refusal before a protected write | Missing scope and a Decision API denial produced zero downstream beta writes. Scrubber failure reached neither the agent nor beta. The engine tests reject malformed or ambiguous decision responses, and trust preflight rejects an untrusted destination before dispatch. [RC refusal scenario](../../internal/server/v1_acceptance_test.go) · [Decision tests](../../internal/engine/http_effects_test.go) · [Trust tests](../../internal/server/trust_test.go) | Verify that the active policies and required obligations match the intended processing. |
| PII through hooks and responses | In the controlled scenario the agent received scrubbed text and beta stored the scrubbed summary. Tests check synthetic email markers in OAuth tool and task responses. The built-in mask also recognizes selected sensitive field names and credential patterns. [RC scenario](../../internal/server/v1_acceptance_test.go) · [OAuth tests](../../internal/server/oauth_integration_test.go) · [Mask implementation](../../internal/expr/eval.go) | Agree on allowed fields, recipient trust, representative PII formats, any unmasked scopes, declassification and retention. Review each external processor that receives protected data. |
| Errors, logs, traces, metrics and audit | Tenant tests check API replies plus retained traces, effects, audit and metrics for synthetic PII and bearer tokens. Storage tests inspect trace and effect rows for recognizable sensitive values. Metrics and OTLP tests check fixed labels, and the audit test verifies that HCL and bearer values stay out of the ledger. [Tenant scenario](../../internal/server/v1_tenant_test.go) · [Storage diagnostics](../../internal/storage/diagnostic_redaction_test.go) · [Telemetry tests](../../internal/server/observability_test.go) · [Audit tests](../../internal/server/audit_test.go) | Review process and proxy logs, external collectors and downstream service logs under the intended settings. Assess administrator access and retention of state, logs and backups. |

The [security boundaries](../security.md) describe the RC's defaults and
sensitive state. The security and privacy reviewer records approved settings,
data categories, residual risks and the approval decision in
[#10](https://github.com/burkostya/regate/issues/10). Results with synthetic
data support that review.

## Independent audit custody

The RC verified that a later SQLite snapshot extended a signed checkpoint.
After RustFS restarted, deletion of the locked version returned HTTP 403.
The test retrieved that version and verified its signature. For #10, the
independent custodian provides evidence of signing key custody, retention
settings, off-host storage and verified retrieval. Follow the
[off-host checkpoint procedure](telemetry.md#off-host-audit-checkpoints)
and attach the result to [#10](https://github.com/burkostya/regate/issues/10).
