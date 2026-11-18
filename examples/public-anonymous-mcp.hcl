# Live third-party MCP interoperability fixture; no accounts or credentials.
# All published tools are read-only. Never send private repository names,
# credentials, or production payloads to these public documentation services.
#
# Explicit egress allowlist for running this configuration:
#   REGATE_ALLOWED_HOSTS=learn.microsoft.com,mcp.deepwiki.com
regate {
  listen = "127.0.0.1:8080"
}

upstream "mslearn" {
  url        = "https://learn.microsoft.com/api/mcp"
  timeout_ms = 30000
}

upstream "deepwiki" {
  url        = "https://mcp.deepwiki.com/mcp"
  timeout_ms = 30000
}

surface "mslearn" {
  path = "/mcp/mslearn"
  tool "search_docs" {
    source      = "mslearn.microsoft_docs_search"
    read_only   = true
    description = "Read-only search across public Microsoft Learn documentation"
  }
}

surface "deepwiki" {
  path = "/mcp/deepwiki"
  tool "wiki_structure" {
    source      = "deepwiki.read_wiki_structure"
    read_only   = true
    description = "Read-only public GitHub repository wiki structure"
  }
}


# A single published typed virtual tool composes both independent public
# providers. Read-only calls share one concurrent execution wave and an
# all-predecessor join. Only bounded counts of returned content blocks are
# published; no vendor response body is echoed into the public result.
pipeline "public_documentation_probe" {
  entry       = "request"
  timeout_ms  = 90000
  concurrency = 2

  node "request" {
    kind = "input"
    input_schema = {
      type = "object"
      required = ["query", "repoName"]
      additionalProperties = false
      properties = {
        query    = {type = "string", minLength = 1, maxLength = 140}
        repoName = {type = "string", minLength = 1, maxLength = 140}
      }
    }
  }
  node "docs" {
    kind      = "mcp.call"
    upstream  = "mslearn"
    tool      = "microsoft_docs_search"
    effect    = "read"
    arguments = {query = "input.query"}
    output_ports = {
      content = {type = "array", minItems = 1}
    }
  }
  node "wiki" {
    kind      = "mcp.call"
    upstream  = "deepwiki"
    tool      = "read_wiki_structure"
    effect    = "read"
    arguments = {repoName = "input.repoName"}
    output_ports = {
      content = {type = "array", minItems = 1}
    }
  }
  node "both" {
    kind = "join"
    mode = "all"
  }
  node "result" {
    kind = "output"
    value = {
      microsoft_blocks = "size(nodes.docs.mcp.content)"
      deepwiki_blocks  = "size(nodes.wiki.mcp.content)"
    }
    output_schema = {
      type = "object"
      required = ["microsoft_blocks", "deepwiki_blocks"]
      additionalProperties = false
      properties = {
        microsoft_blocks = {type = "integer", minimum = 1}
        deepwiki_blocks  = {type = "integer", minimum = 1}
      }
    }
  }
  edge {
    from = "request"
    to   = "docs"
  }
  edge {
    from = "request"
    to   = "wiki"
  }
  edge {
    from = "docs"
    to   = "both"
  }
  edge {
    from = "wiki"
    to   = "both"
  }
  edge {
    from = "both"
    to   = "result"
  }
}

surface "combined" {
  path = "/mcp/combined"
  tool "public_docs_check" {
    pipeline    = "public_documentation_probe"
    description = "Read-only parallel public documentation health check across Microsoft Learn and DeepWiki"
    read_only   = true
    input_schema = {
      type = "object"
      required = ["query", "repoName"]
      additionalProperties = false
      properties = {
        query    = {type = "string", minLength = 1, maxLength = 140}
        repoName = {type = "string", minLength = 1, maxLength = 140}
      }
    }
    output_schema = {
      type = "object"
      required = ["microsoft_blocks", "deepwiki_blocks"]
      additionalProperties = false
      properties = {
        microsoft_blocks = {type = "integer", minimum = 1}
        deepwiki_blocks  = {type = "integer", minimum = 1}
      }
    }
  }
}
