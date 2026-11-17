regate {
  listen = "127.0.0.1:8180"
}

pipeline "echo" {
  entry = "source"

  node "source" {
    kind = "input"
  }
  node "done" {
    kind  = "output"
    value = "nodes.source.value"
  }

  edge {
    from = "source"
    to   = "done"
  }
}

surface "local" {
  path = "/mcp/local"
  tool "echo" {
    pipeline  = "echo"
    read_only = true
  }
}
