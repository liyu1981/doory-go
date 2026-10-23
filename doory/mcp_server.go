// MCP server core: MCPServer (name/title/description/instructions/version/
// website), tool/resource/prompt registries and the JSON-RPC 2.0 dispatch for
// the Streamable HTTP subset we port first (initialize, notifications/initialized,
// tools/list, tools/call, resources/list, resources/read, prompts/list,
// prompts/get). Transport: stateless JSON responses (json_response=True).

package doory

import (
	"encoding/json"
	"net/http"
)

// McpToolSpec is one @tool registration (registry.ToolSpec).
type McpToolSpec struct {
	Name        string
	Title       string
	Description string
	Scope       string
	ReadOnly    bool
	Destructive bool
	Idempotent  bool
	Annotations struct {
		Title           string `json:"title"`
		ReadOnlyHint   bool   `json:"readOnlyHint"`
		DestructiveHint bool  `json:"destructiveHint"`
		IdempotentHint bool   `json:"idempotentHint"`
		OpenWorldHint  bool   `json:"openWorldHint"`
	} `json:"annotations"`
	Handler func(args map[string]any) (any, error)
}

// McpResourceSpec is one @resource registration (registry.ResourceSpec).
type McpResourceSpec struct {
	URI      string
	Name     string
	Title    string
	MimeType string
	Scope    string
	Handler  func() (any, error)
}

// McpPromptSpec is one @prompt registration.
type McpPromptSpec struct {
	Name        string
	Title       string
	Description string
	Handler     func() (any, error)
}

// McpServer is the doory MCP server (registry.build_server -> MCPServer).
type McpServer struct {
	Name          string
	Title         string
	Description   string
	Instructions string
	Version       string
	WebsiteURL    string
	Tools         []McpToolSpec
	Resources     []McpResourceSpec
	Prompts       []McpPromptSpec
}

// NewMcpServer builds the doory MCP server from the probed registry surface
// (apps/mcp/registry.py: TOOLS/RESOURCES/PROMPTS; tool bodies land in a later
// slice and attach here).
func NewMcpServer() *McpServer {
	s := &McpServer{
		Name:          "doory",
		Title:         "Doory",
		Description:  "Create and edit designs in Doory.",
		Instructions: "Doory is a design editor (posters, social posts, slides, cards...). These tools act on the signed-in user's designs.",
		Version:       "1",
		WebsiteURL:    "http://localhost:5173",
	}
	for i := range mcpToolDefs() {
		d := mcpToolDefs()[i]
		spec := McpToolSpec{Name: d.Name, Title: d.Title, Description: d.Description, Scope: d.Scope, ReadOnly: d.ReadOnly, Destructive: d.Destructive, Idempotent: d.Idempotent}
		spec.Annotations.Title = d.Annotations.Title
		spec.Annotations.ReadOnlyHint = d.Annotations.ReadOnlyHint
		spec.Annotations.DestructiveHint = d.Annotations.DestructiveHint
		spec.Annotations.IdempotentHint = d.Annotations.IdempotentHint
		spec.Annotations.OpenWorldHint = d.Annotations.OpenWorldHint
		spec.Handler = func(args map[string]any) (any, error) {
			return nil, &mcpToolError{Message: "Tool body not ported yet: " + d.Name}
		}
		s.Tools = append(s.Tools, spec)
	}
	for i := range mcpResources() {
		r := mcpResources()[i]
		s.Resources = append(s.Resources, r)
	}
	for i := range mcpPrompts() {
		p := mcpPrompts()[i]
		s.Prompts = append(s.Prompts, p)
	}
	return s
}

// jsonrpcMessage is the MCP JSON-RPC 2.0 envelope we accept.
type jsonrpcMessage struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

// isNotification reports whether the envelope carries no id (MCP
// notification; the spec types are "notifications/*").
func (m jsonrpcMessage) isNotification() bool { return len(m.ID) == 0 }

func writeMcpJSON(w http.ResponseWriter, status int, msg map[string]any) {
	b, _ := json.Marshal(msg)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// handleMcpJSONRPC answers one JSON-RPC 2.0 message. Returns whether a
// response was/will be written (notifications answer 202 Accepted with no
// body).
func (s *McpServer) handleMcpJSONRPC(w http.ResponseWriter, msg jsonrpcMessage) bool {
	switch msg.Method {
	case "initialize":
		if msg.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		result := map[string]any{
			"capabilities": map[string]any{
				"prompts": map[string]any{"listChanged": false},
				"resources": map[string]any{"listChanged": false, "subscribe": false},
				"tools":     map[string]any{"listChanged": false},
			},
			"instructions":    s.Instructions,
			"protocolVersion": "2025-06-18",
			"serverInfo": map[string]any{
				"description": s.Description,
				"name":        s.Name,
				"title":       s.Title,
				"version":     s.Version,
			},
			"websiteUrl": s.WebsiteURL,
		}
		writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		return true
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return true
	case "tools/list":
		if msg.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		tools := make([]map[string]any, 0, len(s.Tools))
		for _, t := range s.Tools {
			tools = append(tools, map[string]any{
				"name":         t.Name,
				"title":        t.Title,
				"description": t.Description,
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				"outputSchema": map[string]any{"type": "object", "additionalProperties": true},
				"annotations": map[string]any{
					"title":           t.Annotations.Title,
					"readOnlyHint":    t.Annotations.ReadOnlyHint,
					"destructiveHint": t.Annotations.DestructiveHint,
					"idempotentHint":  t.Annotations.IdempotentHint,
					"openWorldHint":   t.Annotations.OpenWorldHint,
				},
				"scope": t.Scope,
			})
		}
		writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"tools": tools}})
		return true
	case "tools/call":
		if msg.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		var idNum json.Number
		if err := json.Unmarshal(msg.ID, &idNum); err != nil {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		name, _ := msg.Params["name"].(string)
		args, _ := msg.Params["arguments"].(map[string]any)
		if name == "" {
			writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32602, "message": "Invalid params: missing tool name"}})
			return true
		}
		var handler *McpToolSpec
		for i := range s.Tools {
			if s.Tools[i].Name == name {
				handler = &s.Tools[i]
				break
			}
		}
		if handler == nil {
			writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"content":  []map[string]any{{"type": "text", "text": "Unknown tool."}},
				"isError": true,
			}})
			return true
		}
		out, terr := handler.Handler(args)
		if terr != nil {
			writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"content":  []map[string]any{{"type": "text", "text": terr.Error()}},
				"isError": true,
			}})
			return true
		}
		b, _ := json.Marshal(out)
		var content []map[string]any
		content = append(content, map[string]any{"type": "text", "text": string(b)})
		writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
			"content": content, "isError": false, "structuredContent": out,
		}})
		return true
	case "resources/list":
		if msg.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		var idNum json.Number
		if err := json.Unmarshal(msg.ID, &idNum); err != nil {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"resources": []any{}}})
		return true
	case "prompts/list":
		if msg.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		var idNum json.Number
		if err := json.Unmarshal(msg.ID, &idNum); err != nil {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"prompts": []any{}}})
		return true
	default:
		if msg.isNotification() {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		var idNum json.Number
		if err := json.Unmarshal(msg.ID, &idNum); err != nil {
			w.WriteHeader(http.StatusAccepted)
			return true
		}
		writeMcpJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "Method not found: " + msg.Method}})
		return true
	}
}
