# Operational threat review — local engineering scope (#9)

Status: reviewed for the bounded alpha/beta and unauthenticated-read-only provider acceptance stand. **This is not an external production security audit**; #10 still owns complete, non-reduced v1 release acceptance. External durable writable provider attestation was explicitly deferred to #106.

| Threat / abuse case | Implemented defense | Repeatable evidence | Residual risk |
|---|---|---|---|
| Remote actor saturates public MCP | Fixed in-flight public slots, bounded body, per-peer socket-IP token bucket; reject before effects | `TestHTTPAdmissionDoesNotStarveAdminOrBreakStreaming`, `TestRateAdmissionNeverDispatchesDeniedPublicCallsOrConsumesAdminQuota` | Distributed DoS; reverse proxy per-subject controls remain an operator responsibility |
| Remote actor spoofs forwarding headers | Ignore X-Forwarded-For/Forwarded, socket peer only; use memory-only salted SHA-256 of normalized IP | `TestPeerQuotaSameIPDifferentPortsAndIndependentPeers` | Behind a trusted reverse proxy, all end users share one quota unless the proxy applies its own trusted limits |
| Many source IPs exhaust limiter metadata | Fixed max 1024 peers; reject new identities when full and clean stale entries | `TestRateQuotasBoundTrackingAndRejectMalformedConfiguration` | Legitimate new clients may receive 429 under adversarial exhaustion |
| MCP request abuse or HTTP chunked request overflow | Hard 4 MiB request limit plus strict JSON frame/duplicate key validation; 2 MiB admin object cap | `TestHTTPBodyCeilingWithoutEffectDispatch`, `FuzzStrictAdminJSONObjectFraming` | SSE transport flood and upstream response limits require further multi-tenant testing |
| Operational metrics exfiltrate PII | Only bounded category labels/status buckets/aggregate time; no raw path/query/header, inputs, MCP methods or actor | `TestHTTPMetricsPrivateAndFixedCardinality`, `TestOTLPTraceExportContainsOnlyBoundedLabels` | External collectors need controlled retention and TLS/host allowlisting |
| Telemetry exporter outage blocks effect safety | Async bounded 256-span queue; 32-span batches, 2-second timeout, drop counter; egress DNS/redirect/proxy protections | `TestCollectorQueueOutageIsBoundedAndCountersRemainPrivate`, OTLP integration test | Spans may be dropped; telemetry not an authoritative effect journal |
| Local audit record modification or deletion | SQLite v6 append-only-by-API chain with verify-on-read and verify-on-startup; before-mutation barrier | `TestAuditChainRestartParallelAndTamperRefusal` | Privileged DB editor can recompute a plain hash chain; off-host signed/WORM anchor required |
| Arbitrary credentials/data leak through management logs | Audit taxonomy allowlist (category/action/outcome), never accepts free-text/payload; admin bearer protected | `TestManagementMutationsCreateRedactedAuditedBeforeAndAfterRecords` | Full label-aware PII taint proof and signed actor identity still need external review |
| Uncertain external write blindly retried after timeout/lost response | Durable journal, contract-bound positive readback, provider-native idempotency/fence/tombstone in disposable alpha/beta OS processes | `TestTwoProcessMCPDurableProviderContracts`, `TestTwoProcessMCPReadLoadIsolation` | Independent remote vendor's durable-write semantics not attested: #106 |
| Decision deny races a write | Mandatory policy compiler dominance and runtime check before effects; no bypass | `TestTwoProcessMCPReadLoadIsolation` + existing DAG/TLC checks | Broader cross-tenant live IdP/PII/agent end-to-end attestation remains #10 |

## Operator checks

Run `go test -race ./...`, `go vet ./...`, `go build ./cmd/regate ./cmd/regate-demo`, `go test -race ./internal/server -run TestTwoProcessMCP -count=1 -v`. Scheduled GitHub Actions also runs `TestTwoProcessMCPReadLoadIsolation` and an explicit 20-second JSON framing fuzz campaign; Go normal tests run fuzz seeds. Set distinct secrets in the environment and never put production data in these tests.

Quota environment variables: `REGATE_PUBLIC_RPS` (default 300), `REGATE_PUBLIC_BURST` (600), `REGATE_ADMIN_RPS` (100), `REGATE_ADMIN_BURST` (200), each 1..10000. Limits are per process/remote socket IP, not a distributed policy. Both limiter maps are bounded to 1024 peer digests; they are ephemeral and not audit principals. A saturated limiter returns HTTP 429 before a tool can dispatch.

## Formal exit

These adversarial tests and this *local engineering review* are useful evidence, but **do not** claim comprehensive production safety without load under realistic topology, distributed budgets, signed audit custody, pen test, third-party write attestation, authentication/consent test and security signoff. Those stay in #9/#10/#106 until explicitly accepted.
