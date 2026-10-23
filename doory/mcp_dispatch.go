// MCP Streamable HTTP entry: POST /mcp JSON-RPC 2.0 body (<= MCP_MAX_REQUEST_BYTES)
// -> jsonrpcMessage -> MCPServer.handleMcpJSONRPC.

package doory

import (
	"encoding/json"
	"net/http"
)

// mcpDispatch ports the Streamable HTTP transport (stateless_http=True,
// json_response=True): parse one JSON-RPC message, answer via the server.
func (a *App) mcpDispatch(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(a.Settings.McpMaxRequestBytes)))
	var msg jsonrpcMessage
	if err := dec.Decode(&msg); err != nil {
		writeMcpJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "Parse error"},
		})
		return
	}
	s := NewMcpServer()
	s.handleMcpJSONRPC(w, msg)
}
