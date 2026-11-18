# LIVE NETWORK TEST FIXTURE ONLY. This vendor publicly documents ALL write
# operations as simulations. It stores no global mutations. Do not reuse this
# HCL for a production endpoint or switch the upstream to a real write API.
# Run only with REGATE_ALLOWED_HOSTS=api.sondahub.com and loopback listener.
regate {
  listen = "127.0.0.1:8080"
}

upstream "sondahub" {
  url = "https://api.sondahub.com/mcp"
  timeout_ms = 30000
}

# Arguments are literal CEL strings, not dynamic user input. The source is
# the only executable authority for the synthetic provider write intent.
# A successful remote tool result must carry its simulation note.
pipeline "sandbox_create" {
  entry = "entry"
  timeout_ms = 50000
  node "entry" {
    kind = "input"
    input_schema = {type = "object", additionalProperties = false}
  }
  node "write" {
    kind = "mcp.call"
    upstream = "sondahub"
    tool = "create_record"
    effect = "write"
    arguments = {
      api = "literal:store"
      collection = "literal:categories"
      record = {
        name = "literal:Regate synthetic MCP acceptance"
        slug = "literal:regate-acceptance-synthetic"
      }
    }
  }
  node "done" {
    kind = "output"
    value = {simulation_note_present = "nodes.write.value._note != ''"}
    output_schema = {
      type = "object"
      required = ["simulation_note_present"]
      additionalProperties = false
      properties = {simulation_note_present = {type = "boolean", const = true}}
    }
  }
  edge {
    from = "entry"
    to = "write"
  }
  edge {
    from = "write"
    to = "done"
  }
}

# Missing required store.categories.slug intentionally triggers provider's
# MCP application-level isError=true. An attempted write is conservatively
# UNKNOWN, not retried, even when the provider reports validation failure.
pipeline "sandbox_rejected" {
  entry = "entry"
  timeout_ms = 50000
  node "entry" {
    kind = "input"
    input_schema = {type = "object", additionalProperties = false}
  }
  node "write" {
    kind = "mcp.call"
    upstream = "sondahub"
    tool = "create_record"
    effect = "write"
    arguments = {
      api = "literal:store"
      collection = "literal:categories"
      record = {
        name = "literal:Regate intentionally invalid synthetic category"
      }
    }
  }
  node "done" {
    kind = "output"
    value = "nodes.write.value"
  }
  edge {
    from = "entry"
    to = "write"
  }
  edge {
    from = "write"
    to = "done"
  }
}

surface "sandbox" {
  path = "/mcp/sandbox"
  tool "create_fixture" {
    pipeline = "sandbox_create"
    description = "Test-only simulated category create; no provider storage"
    read_only = false
    input_schema = {type = "object", additionalProperties = false}
    output_schema = {
      type = "object"
      required = ["simulation_note_present"]
      additionalProperties = false
      properties = {simulation_note_present = {type = "boolean", const = true}}
    }
  }
  tool "reject_fixture" {
    pipeline = "sandbox_rejected"
    description = "Test-only intentionally schema-invalid simulated category create"
    read_only = false
    input_schema = {type = "object", additionalProperties = false}
  }
}
