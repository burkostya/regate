# Local acceptance only. The test replaces these loopback addresses and
# creates disposable provider and gateway state.
regate {
  listen = "127.0.0.1:9200"
}

oauth "acceptance" {
  issuer   = "https://issuer.test"
  audience = "https://regate.test/mcp/acceptance"
  jwks_url = "http://127.0.0.1:9203/jwks"
}

upstream "alpha" {
  url         = "http://127.0.0.1:9201/mcp"
  trust_level = "restricted"
}

upstream "beta" {
  url         = "http://127.0.0.1:9202/mcp"
  trust_level = "restricted"
}

upstream "agent" {
  url         = "http://127.0.0.1:9203/mcp"
  trust_level = "restricted"
}

hook "decision" {
  transport   = "http"
  url         = "http://127.0.0.1:9201/decision"
  contract    = "decision"
  on_error    = "deny"
  trust_level = "restricted"
}

hook "scrubber" {
  transport   = "http"
  url         = "http://127.0.0.1:9203/redact"
  contract    = "transform"
  on_error    = "deny"
  trust_level = "restricted"
}

hook "summary_agent" {
  transport   = "mcp"
  upstream    = "agent"
  tool        = "summarize"
  contract    = "transform"
  trust_level = "restricted"
}

policy "publish_permission" {
  decision_hook = "decision"
}

# The provider persists an operation token with the immutable record. An ID
# or value match alone cannot prove that this specific write was applied.
readback "beta_record" {
  write_operation      = "beta.put_record"
  upstream             = "beta"
  read_tool            = "get_record"
  intent_pointer       = "/record_id"
  read_argument        = "record_id"
  result_pointer       = "/structuredContent/record/record_id"
  match_intent_pointer = "/value"
  match_result_pointer = "/structuredContent/record/value"
  write_token_argument = "operation_token"
  result_token_pointer = "/structuredContent/record/operation_token"
  safe_read            = true
}

pipeline "publish_summary" {
  entry       = "authorize"
  timeout_ms  = 15000
  concurrency = 2

  node "authorize" {
    kind      = "policy.check"
    policy    = "publish_permission"
    mandatory = true
    value     = "input"
  }

  node "require_approval" {
    kind = "condition"
    test = "has(input.approval_required) && input.approval_required"
  }

  node "approval" {
    kind          = "suspend"
    output_schema = { type = "boolean", const = true }
  }

  node "fetch" {
    kind      = "mcp.call"
    upstream  = "alpha"
    tool      = "get_record"
    effect    = "read"
    arguments = { record_id = "input.record_id" }
    mock      = { record = { value = "Synthetic preview" } }
  }

  node "redact" {
    kind  = "hook.call"
    hook  = "scrubber"
    value = "{'notes': nodes.fetch.value.record.value}"
  }

  node "summarize" {
    kind  = "hook.call"
    hook  = "summary_agent"
    value = "nodes.redact.value"
    output_schema = {
      type       = "object"
      properties = { notes = { type = "string" } }
      required   = ["notes"]
    }
  }

  node "label" {
    kind          = "transform"
    value         = "literal:Summary for "
    output_schema = { type = "string" }
  }

  node "join" {
    kind  = "join"
    mode  = "all"
    value = "ports.label + input.record_id + ': ' + ports.agent.notes"
    input_ports = {
      label = { type = "string" }
      agent = {
        type       = "object"
        properties = { notes = { type = "string" } }
        required   = ["notes"]
      }
    }
    output_schema = { type = "string" }
  }

  node "write" {
    kind     = "mcp.call"
    upstream = "beta"
    tool     = "put_record"
    effect   = "write"
    mock     = { record = { record_id = "preview" } }
    arguments = {
      record_id = "input.record_id"
      value     = "nodes.join.value"
      fault     = "input.lose_reply ? 'drop_after_commit' : ''"
    }
  }

  node "done" {
    kind  = "output"
    value = "{'record_id': nodes.write.value.record.record_id}"
  }

  edge {
    from = "authorize"
    to   = "require_approval"
  }
  edge {
    from = "require_approval"
    to   = "approval"
    when = "true"
  }
  edge {
    from = "require_approval"
    to   = "fetch"
    when = "false"
  }
  edge {
    from = "require_approval"
    to   = "label"
    when = "false"
  }
  edge {
    from = "approval"
    to   = "fetch"
  }
  edge {
    from = "approval"
    to   = "label"
  }
  edge {
    from = "fetch"
    to   = "redact"
  }
  edge {
    from = "redact"
    to   = "summarize"
  }
  edge {
    from = "summarize"
    to   = "join"
  }
  edge {
    from = "label"
    to   = "join"
  }
  edge {
    kind    = "data"
    from    = "summarize"
    to      = "join"
    to_port = "agent"
  }
  edge {
    kind    = "data"
    from    = "label"
    to      = "join"
    to_port = "label"
  }
  edge {
    from = "join"
    to   = "write"
  }
  edge {
    from = "write"
    to   = "done"
  }
}

surface "acceptance" {
  path            = "/mcp/acceptance"
  oauth           = "acceptance"
  required_scopes = ["acceptance:access"]

  tool "publish_summary" {
    pipeline            = "publish_summary"
    description         = "Publish a scrubbed summary to the disposable beta fixture"
    required_scopes     = ["summary:write"]
    data_classification = "restricted"
    input_schema = {
      type = "object"
      properties = {
        record_id         = { type = "string" }
        lose_reply        = { type = "boolean" }
        approval_required = { type = "boolean" }
      }
      required             = ["record_id", "lose_reply"]
      additionalProperties = false
    }
    output_schema = {
      type                 = "object"
      properties           = { record_id = { type = "string" } }
      required             = ["record_id"]
      additionalProperties = false
    }
  }
}
