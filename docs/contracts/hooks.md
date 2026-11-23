# Hook contracts and lifecycle

## Goal

Hooks allow transform services, Decision APIs, external agent MCP tools and
observers to participate in tool execution. The gateway enforces the
configured permissions even when a hook returns an allow decision.

## Sensitive data transfer

The data owner determines which fields and values are sensitive and who may
receive them. The configuration author expresses that policy in HCL with
caller scopes, destination trust declarations, decision checks, and explicit
mappings or transforms. The same policy can cover internal prices, contract
text, credentials and personal data.

Use a local CEL transform to build a payload from the fields approved for
the destination. For example, the payload for an external billing tool can
contain only the order ID and public price. Connect the call to the
transformed value and validate its shape where a schema is available.
A denied decision or a failed required transform blocks the dependent
call. Apply the same rules before returning results to MCP clients.

HTTP and MCP hooks, including external redactors and decision services,
are destinations in their own right. Authorize the input sent to each hook.
The local `mask.pii` node is one example of a transform. It matches selected
patterns and field names. Use explicit payload mappings for fields that the
owner's policy protects, including arbitrary business data.
[OAuth trust and response rules](oauth.md#restricted-data-trust-boundary)

## Hook transports

1. `builtin`: local Go transformer with pattern-based masking.
2. `http`: HTTP POST JSON `{ "input": value }`, optional `auth_env`, request timeout and JSON response.
3. `mcp`: `tools/call` on a configured upstream MCP tool, with JSON object arguments.

### Decision result

```json
{"decision":"allow","obligations":[]}
```

Any response other than explicit `allow`, including network timeout or `indeterminate`, is denied. If `obligations` is nonempty, the prototype fails closed because it cannot prove the obligations were enforced.

### Transform result

```json
{"action":"replace","value":{"customer_id":"X", "email":"[REDACTED]"}}
```

The runtime returns the `value`. A hook returning `{ "action":"deny" }` aborts. Other control actions such as `route`/`retry`/`suspend` are **not** part of the implemented hook contract despite being planned for complete v1.

## Interceptor lifecycle

- `on_request`: invoked on public tool arguments before routing.
- `before_call`: invoked before outbound MCP or HTTP call; for MCP tool calls the result must be an object.
- `after_call`: transforms the external result; if changed, the raw MCP response must not be copied unchanged to the client.
- `before_response`: transforms the final value.
- `on_error`: best-effort observer on an error; not allowed to suppress or replace the original error.

Interceptors are sorted by `order` and then `name`, optionally scoped to surface and tool. A missing or failing interceptor fails closed (except the observer hook, which cannot replace the original failure). The UI should always show their placement relative to explicit graph nodes.

## Security considerations

- Place the trusted mandatory policy check before reading or transmitting protected data.
- HTTP and MCP hooks are external transfers. They need independent trust, authentication and effect contracts.
- Keep sensitive values out of diagnostics, including request bodies, response
  bodies and error text. Review retained output against the owner's policy.
- Verify the configured mapping, recipient and authorization with
  representative inputs and a denied or failed path before activation.


## Resuming a suspended pipeline (PR #59)

The final public response boundary is shared by the initial `Run` and
durable `Resume` paths. A still-suspended execution does **not** run
`before_response`. When a resumed root completes, Regate enforces
root-output exclusivity, validates the public tool's `output_schema`,
runs the ordered `before_response` interceptors, and validates the
transformed result again. A changed result clears an earlier MCP envelope;
failed validation or a failed hook clears all public result fields and
marks the run failed.

Once the graph completes, the consumed durable checkpoint is deleted even
when its final response schema or response hook fails. It cannot be
resumed again to replay an already dispatched external HTTP/MCP effect;
unknown hook outcomes instead remain in the effect journal for explicit
reconciliation. Failures are traced with redacted diagnostics and the
best-effort `on_error` observer. This does **not** mean an external
effect has been rolled back, and a partially completed *graph* that
fails before reaching the final response boundary still follows its
separate effect reconciliation policy.

## Strict HTTP JSON response boundary (#9)

HTTP hook responses must be exactly one JSON object of at most 4 MiB,
including trailing whitespace. Nested or escaped-equivalent duplicate
JSON members, invalid UTF-8 and excessive nesting are rejected before
evaluating Decision API `allow` or publishing a transform result.
An invalid, missing or oversized response after a POST is journaled as
an **unknown** external effect: it cannot be blindly retried even if
the remote service already committed a write. Only a valid response
marks the HTTP POST effect confirmed. This is a transport-level
framing guarantee, not verification of a remote Decision provider's
trustworthiness.

## Outbound HTTP hook request limit (#9)

The encoded `{"input": ...}` request body is limited to 4 MiB,
including the entire JSON envelope. This limit is enforced after
transformations and before an HTTP write is admitted into the effect
journal or sent over the network. Oversized payloads therefore cannot
create an external effect or an ambiguous write outcome. The limit is
separate from, and equally strict as, the 4 MiB response framing limit.

## Resource and prompt interceptors

An interceptor may select a published `resource` by HCL block name or a
published `prompt` by MCP name. `surface` limits the match to one assembled
surface. With no surface selector the rule matches that name on every
assembled surface. Existing `tool` selectors and interceptors without an
object selector keep their tool semantics. Transparent upstream surfaces
and the management surface do not run these interceptors.

Resource and prompt interceptors choose one action.

- `hook` calls a configured builtin, HTTP or MCP hook. A decision hook
  must return explicit `allow` with no outstanding obligations. A transform
  replaces the current stage value.
- `policy` resolves the named policy's external decision hook. A successful
  decision keeps the original stage value.
- `check` evaluates a CEL Boolean. False, an evaluation error or an
  unexpected type stops the operation.
- `transform` evaluates CEL to produce the next stage value.

An optional `input` CEL projection limits the fields sent to a decision or
transform hook. CEL receives the stage value as `input` and the effective
MCP request as `request`. Neither contains request headers, credentials, or
transport metadata. Rules within a stage run by `order`, then name.

| Stage | Value |
| --- | --- |
| `on_request` | `{uri}` for a resource, `{name, arguments}` for a prompt |
| `before_call` | Same request shape, immediately before a configured upstream read or get |
| `after_call` | The upstream MCP result object |
| `before_response` | The final resource `contents` or prompt `messages` result object |
| `on_error` | Fixed diagnostic fields `status`, `message`, and `run_id` |

`before_call` and `after_call` require an upstream source. The incoming
resource URI and public prompt name remain fixed through request
transformations. Sourced prompts call the configured upstream name with the
transformed string arguments. Surface and object scopes and the trust of
destinations are checked before dispatching hooks or upstream requests.
External hooks use the existing token exchange and durable effect boundary.
A lost response leaves a potentially effectful hook in an unknown state for
reconciliation. Error observers are best effort; each receives only the
fixed diagnostic fields. They cannot change the result or authorize an
operation.

OAuth content is masked after the response stages. Opaque resource blobs,
embedded blobs, images and audio cannot be returned on OAuth surfaces.
Intercepted or OAuth-protected resource results advertise a private cache
scope and zero freshness to require clients to revalidate policy on the
next read.

The [content interceptors example](../../examples/content-interceptors.hcl)
shows a resource with masked text and a prompt with validated arguments and
a generated response.
