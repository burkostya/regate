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
