# Anonymous third-party HTTP MCP interoperability

This is **not** the local two-process MCP read/write fixture. These remote
servers are independently maintained third-party implementations, connected
without an account, cookies, bearer token, API key, or stdio transport:

| Provider | Operator | Streamable HTTP URL | Test tool | Usage |
| --- | --- | --- | --- | --- |
| Microsoft Learn | Microsoft | https://learn.microsoft.com/api/mcp | `microsoft_docs_search` | Public documentation only |
| DeepWiki | Cognition/Devin | https://mcp.deepwiki.com/mcp | `read_wiki_structure` | Public GitHub repos only |

Official references:
- https://learn.microsoft.com/en-us/training/support/mcp — documents **no authentication required**, Streamable HTTP, and the public endpoint
- https://docs.devin.ai/work-with-devin/deepwiki-mcp — documents **no authentication required**, Streamable HTTP `/mcp`, and its public wiki tools

## Run on demand

The normal Go suite skips the public integration test. To execute against
the live internet, from the repository root:

```sh
REGATE_TEST_PUBLIC_MCP=1 go test -race ./internal/server \
  -run '^TestPublicAnonymousMCPProviders$' -count=1 -timeout 4m -v
```

The test creates an isolated temporary SQLite state directory and an
in-process Regate HTTP server, but it connects to the **actual external**
Microsoft Learn and DeepWiki MCP services over TLS. It uses the official
Go MCP SDK for both incoming and outgoing MCP traffic.

It checks (1) anonymous upstream initialization and tool discovery,
(2) Regate's strict curation of one read-only tool on each of two isolated
public HTTP MCP surfaces, (3) one actual read-only tool call to each remote
provider, and (4) zero effects admitted to the write journal.

The fixture in `examples/public-anonymous-mcp.hcl` publishes:
- `/mcp/mslearn`: `search_docs` → `mslearn.microsoft_docs_search`
- `/mcp/deepwiki`: `wiki_structure` → `deepwiki.read_wiki_structure`
- `/mcp/combined`: `public_docs_check` → one typed, concurrent
  `public_documentation_probe` DAG that reads from **both** providers and
  releases a response only after an `all` join.

The combined tool accepts public `query` and `repoName` strings with
bounded JSON Schema constraints; the opt-in live test supplies fixed public
inputs only. Its two `mcp.call` nodes declare `effect = "read"` and explicit
nonempty `content` array contracts. Each vendor's returned content is
validated but **never returned verbatim**: the published schema exposes only
positive `microsoft_blocks` and `deepwiki_blocks` counts. One incoming
`tools/call` now tests two real third-party tool executions in one
parallel Regate wave with a complete-fan-in join. The deterministic Go compiler
test also rejects changing that join from `all` to `any`, because the
terminal references both upstream results.

This is evidence of **real read-only cross-provider composition**, not
evidence that remote MCP providers can safely accept anonymous writes.
The read-only declarations are operator assertions about specific provider
tools, not vendor attestations of read consistency. Never send confidential
payloads, credentials or private repository identities to these services.

The test sets `REGATE_ALLOWED_HOSTS=learn.microsoft.com,mcp.deepwiki.com`
explicitly, honoring Regate's existing outbound DNS/IP security guard.
For a manually launched gateway using this HCL, set that same allowlist in
its environment. Neither server is allowed to issue non-loopback plaintext
HTTP connections. No inbound management operations are exposed without the
separate admin token.

### CI behavior and limits

`.github/workflows/public-anonymous-mcp.yml` runs this test on relevant
PR changes, weekly, and on manual dispatch. Vendor outages, rate limits,
protocol drift, response-size limits, or DNS restrictions may cause this
external test to fail. Such failures are reported, **not** silently replaced
with mocks or retried as writes.

This exercise does not validate remote write idempotency, provider fencing,
reconciliation, OAuth, LLM hooks, PII enforcement, or a full production
release. Keep the independent local read/write stand for deterministic safe
failure and write tests. Do not send secrets or production records to these
third-party endpoints; the only request inputs are fixed public queries.
