package mcp

import (
	"bytes"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/burkostya/regate/internal/strictjson"
)

// boundAndValidateMCPResponse validates complete JSON-RPC HTTP frames before
// the SDK can interpret any duplicate field as last-wins. SSE remains streamed
// with an independent aggregate byte cap; parsing SSE event data is a separate
// protocol boundary.
func boundAndValidateMCPResponse(response *http.Response) error {
	if response == nil || response.Body == nil {
		return errors.New("missing upstream MCP response body")
	}
	mediaType, _, typeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	isJSON := typeErr == nil && (strings.EqualFold(mediaType, "application/json") ||
		strings.HasSuffix(strings.ToLower(mediaType), "+json"))
	// Notifications may validly return 202/204 with no response body.
	if !isJSON || response.StatusCode == http.StatusAccepted ||
		response.StatusCode == http.StatusNoContent {
		response.Body = &limitedBody{ReadCloser: response.Body}
		return nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, MaxResponse+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return errors.New("cannot read upstream MCP JSON response")
	}
	if len(raw) > MaxResponse {
		return errors.New("upstream MCP JSON response exceeds 8 MiB")
	}
	if err := strictjson.Check(raw); err != nil {
		// Never embed raw or potentially sensitive remote JSON in diagnostics.
		return errors.New("invalid upstream MCP JSON response")
	}
	response.Body = io.NopCloser(bytes.NewReader(raw))
	response.ContentLength = int64(len(raw))
	return nil
}
