# Operational threat review — local engineering scope (#9)

The local engineering review covers the alpha/beta, independent read-only
providers and controlled Keycloak and NGINX acceptance profiles. #10 tracks
production acceptance. #106 tracks independent durable writable provider
attestation.

| Threat / abuse case | Implemented defense | Repeatable evidence | Residual risk |
|---|---|---|---|
| Remote actor saturates public MCP | Fixed in-flight public slots, bounded body, per-peer socket-IP token bucket and optional shared OAuth principal quota. Reject before effects. | `TestHTTPAdmissionDoesNotStarveAdminOrBreakStreaming`, `TestRateAdmissionNeverDispatchesDeniedPublicCallsOrConsumesAdminQuota`, `TestV1Soak` | Multi-host DoS protection depends on the target ingress and deployment |
| Remote actor spoofs forwarding headers | Socket peer is derived from the connection. The optional shared quota applies to authenticated OAuth issuer and subject pairs. | `TestPeerQuotaSameIPDifferentPortsAndIndependentPeers`, `TestV1TenantBoundaries` | Unauthenticated callers behind a proxy share its socket-peer quota |
| Many source IPs exhaust limiter metadata | Fixed max 1024 peers; reject new identities when full and clean stale entries | `TestRateQuotasBoundTrackingAndRejectMalformedConfiguration` | Legitimate new clients may receive 429 under adversarial exhaustion |
| MCP request abuse or HTTP chunked request overflow | Hard 4 MiB request limit plus strict JSON frame/duplicate key validation; 2 MiB admin object cap | `TestHTTPBodyCeilingWithoutEffectDispatch`, `FuzzStrictAdminJSONObjectFraming` | SSE transport flood and upstream response limits require further multi-tenant testing |
| Operational metrics exfiltrate PII | Only bounded category labels/status buckets/aggregate time; no raw path/query/header, inputs, MCP methods or actor | `TestHTTPMetricsPrivateAndFixedCardinality`, `TestOTLPTraceExportContainsOnlyBoundedLabels` | External collectors need controlled retention and TLS/host allowlisting |
| Telemetry exporter outage blocks effect safety | Async bounded 256-span queue; 32-span batches, 2-second timeout, drop counter; egress DNS/redirect/proxy protections | `TestCollectorQueueOutageIsBoundedAndCountersRemainPrivate`, OTLP integration test | Spans may be dropped; telemetry not an authoritative effect journal |
| Local audit record modification or deletion | SQLite append-only-by-API audit chain, before-mutation barrier and offline backup verification against a signed checkpoint | `TestAuditChainRestartParallelAndTamperRefusal`, `make test-v1-controlled` | Independent signer custody and off-host immutable retention require deployment and review |
| Arbitrary credentials/data leak through management logs | Audit taxonomy allowlist (category/action/outcome), never accepts free-text/payload; admin bearer protected | `TestManagementMutationsCreateRedactedAuditedBeforeAndAfterRecords` | Full label-aware PII taint proof and signed actor identity still need external review |
| Uncertain external write blindly retried after timeout/lost response | Durable journal, contract-bound positive readback, provider-native idempotency/fence/tombstone in disposable alpha/beta OS processes | `TestTwoProcessMCPDurableProviderContracts`, `TestTwoProcessMCPReadLoadIsolation` | Independent remote vendor's durable-write semantics not attested: #106 |
| Decision deny races a write | Mandatory policy compiler dominance and runtime check before effects; no bypass | `TestTwoProcessMCPReadLoadIsolation` + existing DAG/TLC checks | Broader cross-tenant live IdP/PII/agent end-to-end attestation remains #10 |

## Operator checks

Run `go test -race ./...`, `go vet ./...`, `go build ./cmd/regate ./cmd/regate-demo`, `go test -race ./internal/server -run TestTwoProcessMCP -count=1 -v`. Scheduled GitHub Actions also runs `TestTwoProcessMCPReadLoadIsolation` and an explicit 20-second JSON framing fuzz campaign; Go normal tests run fuzz seeds. Set distinct secrets in the environment and never put production data in these tests.

Quota environment variables: `REGATE_PUBLIC_RPS` (default 300), `REGATE_PUBLIC_BURST` (600), `REGATE_ADMIN_RPS` (100), `REGATE_ADMIN_BURST` (200), each 1..10000. These socket-peer limits are per process. Both limiter maps are bounded to 1024 ephemeral peer digests. The optional SQLite-backed OAuth budget survives same-host process promotion. A saturated limiter returns HTTP 429 before a tool can dispatch. [Quota settings](telemetry.md#http-admission-telemetry-and-privacy-boundaries)

## Formal exit

Production acceptance in #10 requires load in the target topology, external
audit custody, authentication and consent checks, and security and PII
signoff. Independent durable write attestation remains in #106.
