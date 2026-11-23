# Replace this URL with the upstream's existing Streamable HTTP endpoint.
upstream "provider" {
  url = "http://127.0.0.1:9091/mcp"
}

# Switch the client's MCP URL to http://127.0.0.1:8080/mcp/provider.
surface "provider" {
  upstream = "provider"
  auth     = "anonymous"
}
