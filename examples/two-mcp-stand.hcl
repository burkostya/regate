# LOCAL TEST STAND ONLY: two independently run, loopback-only MCP providers.
# All writes are immutable synthetic records in separate, disposable state dirs.
regate {
  listen = "127.0.0.1:9200"
}

upstream "alpha" {
  url        = "http://127.0.0.1:9201/mcp"
  timeout_ms = 5000
}
upstream "beta" {
  url        = "http://127.0.0.1:9202/mcp"
  timeout_ms = 5000
}

hook "test_decision" {
  transport  = "http"
  url        = "http://127.0.0.1:9201/decision"
  contract   = "decision"
  on_error   = "deny"
  timeout_ms = 5000
}
policy "copy_permission" {
  decision_hook = "test_decision"
}

# Positive-only readback: absence is not proof. These identity checks are
# suitable for this immutable, bounded fixture, NOT generic mutable providers.
readback "alpha_record" {
  write_operation = "alpha.put_record"
  upstream        = "alpha"
  read_tool       = "get_record"
  intent_pointer  = "/record_id"
  read_argument   = "record_id"
  result_pointer  = "/structuredContent/record/record_id"
  match_intent_pointer = "/value"
  match_result_pointer = "/structuredContent/record/value"
  # The provider stores this Regate-issued per-effect token with the value.
  write_token_argument = "operation_token"
  result_token_pointer = "/structuredContent/record/operation_token"
  safe_read       = true
}
readback "beta_record" {
  write_operation = "beta.put_record"
  upstream        = "beta"
  read_tool       = "get_record"
  intent_pointer  = "/record_id"
  read_argument   = "record_id"
  result_pointer  = "/structuredContent/record/record_id"
  match_intent_pointer = "/value"
  match_result_pointer = "/structuredContent/record/value"
  # The provider stores this Regate-issued per-effect token with the value.
  write_token_argument = "operation_token"
  result_token_pointer = "/structuredContent/record/operation_token"
  safe_read       = true
}

# Explicit operator-approved inverses. Only the management API may invoke
# them after an original effect was independently resolved as applied.
# The fixture uses immutable record IDs and persists a tombstone instead of
# deleting the original record or its provider operation token.
compensation "alpha_undo" {
  for_operation = "alpha.put_record"
  upstream       = "alpha"
  tool           = "revoke_record"
  intent_pointer = "/record_id"
  argument       = "record_id"
  enabled        = true
}
compensation "beta_undo" {
  for_operation = "beta.put_record"
  upstream       = "beta"
  tool           = "revoke_record"
  intent_pointer = "/record_id"
  argument       = "record_id"
  enabled        = true
}

# A real typed DAG, not a mock endpoint: protect the read and subsequent
# external write with an actual Decision POST; no bypass edge is allowed.
pipeline "copy_record" {
  entry = "authorize"
  timeout_ms = 15000
  concurrency = 2

  node "authorize" {
    kind      = "policy.check"
    policy    = "copy_permission"
    mandatory = true
    value     = "input"
  }
  node "read" {
    kind      = "mcp.call"
    upstream  = "alpha"
    tool      = "get_record"
    effect    = "read"
    arguments = {record_id = "input.record_id"}
  }
  node "write" {
    kind      = "mcp.call"
    upstream  = "beta"
    tool      = "put_record"
    effect    = "write"
    arguments = {
      record_id = "input.record_id"
      value     = "nodes.read.value.record.value"
    }
  }
  node "done" {
    kind  = "output"
    value = "nodes.write.value"
  }
  edge {
    from = "authorize"
    to   = "read"
  }
  edge {
    from = "read"
    to   = "write"
  }
  edge {
    from = "write"
    to   = "done"
  }
}

surface "alpha" {
  path = "/mcp/alpha"
  tool "alpha_put" {
    source      = "alpha.put_record"
    description = "Safe immutable test-only write to alpha"
  }
  tool "alpha_get" {
    source      = "alpha.get_record"
    read_only   = true
    description = "Safe read from alpha"
  }
  tool "alpha_stats" {
    source      = "alpha.stats"
    read_only   = true
    description = "Read alpha's independent attempt counters"
  }
  resource "guide" {
    uri       = "fixture://alpha/guide"
    mime_type = "text/plain"
    text      = "Only synthetic data in alpha's disposable state directory."
  }
  prompt "review" {
    text = "Inspect synthetic alpha records only."
  }
}

surface "beta" {
  path = "/mcp/beta"
  tool "beta_put" {
    source      = "beta.put_record"
    description = "Safe immutable test-only write to beta"
  }
  tool "beta_get" {
    source      = "beta.get_record"
    read_only   = true
    description = "Safe read from beta"
  }
  tool "beta_stats" {
    source      = "beta.stats"
    read_only   = true
    description = "Read beta's independent attempt counters"
  }
  resource "guide" {
    uri       = "fixture://beta/guide"
    mime_type = "text/plain"
    text      = "Only synthetic data in beta's disposable state directory."
  }
  prompt "review" {
    text = "Inspect synthetic beta records only."
  }
}

surface "workflow" {
  path = "/mcp/workflow"
  tool "copy" {
    pipeline    = "copy_record"
    description = "Decision-gated alpha read followed by one beta write"
    input_schema = {
      type = "object"
      properties = {
        record_id = {type = "string"}
      }
      required = ["record_id"]
    }
  }
}
