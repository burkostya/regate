# Regate example: connect two MCP upstreams, gate CRM access with Decision API,
# mask PII, and summarize redacted information through an agent tool.
regate {
  listen = "127.0.0.1:8080"
}

upstream "crm" {
  url        = "http://127.0.0.1:9091/mcp"
  timeout_ms = 5000
}

upstream "agent" {
  url        = "http://127.0.0.1:9091/mcp"
  timeout_ms = 15000
}

hook "decision" {
  transport  = "http"
  url        = "http://127.0.0.1:9091/decision"
  contract   = "decision"
  on_error   = "deny"
  timeout_ms = 5000
}

hook "mask_pii" {
  transport  = "http"
  url        = "http://127.0.0.1:9091/redact"
  contract   = "transform"
  on_error   = "deny"
}

hook "summarizer" {
  transport = "mcp"
  upstream  = "agent"
  tool      = "summarize"
  contract  = "transform"
}

policy "customer_read" {
  decision_hook = "decision"
}

pipeline "customer_summary" {
  entry       = "authorize"
  timeout_ms  = 60000
  concurrency = 4

  node "authorize" {
    kind   = "policy.check"
    policy = "customer_read"
    value  = "input"
  }

  node "fetch" {
    kind      = "mcp.call"
    upstream  = "crm"
    tool      = "get_customer"
    effect    = "read"
    arguments = {customer_id = "input.customer_id"}
    mock      = {customer_id = "C123", name = "Alice", email = "alice@example.com"}
  }

  node "mask" {
    kind  = "hook.call"
    hook  = "mask_pii"
    value = "nodes.fetch.value"
  }

  node "summarize" {
    kind  = "hook.call"
    hook  = "summarizer"
    value = "nodes.mask.value"
  }

  node "finish" {
    kind  = "output"
    value = "nodes.summarize.value"
  }

  edge {
    from = "authorize"
    to   = "fetch"
  }
  edge {
    from = "fetch"
    to   = "mask"
  }
  edge {
    from = "mask"
    to   = "summarize"
  }
  edge {
    from = "summarize"
    to   = "finish"
  }
}

surface "support" {
  path = "/mcp/support"

  tool "summarize_customer" {
    pipeline    = "customer_summary"
    description = "Summarize a CRM customer after access approval and PII masking"
    input_schema = {
      type = "object"
      properties = {
        customer_id = {type = "string"}
      }
      required = ["customer_id"]
    }
  }

  tool "get_customer_direct" {
    source      = "crm.get_customer"
    description = "Direct passthrough to CRM, unchanged MCP content"
    read_only   = true
  }

  resource "instructions" {
    uri       = "regate://support/instructions"
    mime_type = "text/plain"
    text      = "Use summarize_customer for PII-safe summaries."
  }

  prompt "customer_review" {
    description = "Review a customer's records"
    text        = "Summarize this customer's activity without exposing PII."
  }
}
