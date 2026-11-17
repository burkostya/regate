package server

import (
    "io"

    "github.com/burkostya/regate/internal/strictjson"
)

// Management requests have a smaller body cap than public MCP POST.
// Keep this wrapper for the admin boundary and JSON framing fuzz target.
const maxAdminJSONBytes int64 = 2 << 20
const maxAdminJSONDepth = strictjson.MaxDepth

func strictJSONObject(reader io.Reader) (map[string]any,error) {
    return strictjson.ReadObject(reader,maxAdminJSONBytes)
}

func checkUniqueJSONKeys(raw []byte) error {
    return strictjson.Check(raw)
}
