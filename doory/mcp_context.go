// MCP caller context: apps/mcp/context.py (ContextVar Caller(user, token)
// -> current()). The MCP streamable transport binds the bearer-authenticated
// caller for the duration of one request; tools/resources read it via
// current().

package doory

import "context"

// McpCaller is the MCP per-request identity: the user behind a personal
// access token plus the token's scopes (token.scopes) and public label
// (token.name -> label). Mirrors apps/mcp/context.Caller.
type McpCaller struct {
	User   *UserRow
	Token  string
	Scopes []string
	Label  string
}

// Has reports whether the caller's token carries `scope`.
func (c *McpCaller) Has(scope string) bool {
	if c == nil || c.User == nil {
		return false
	}
	for _, s := range c.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// mcpContextKey is the context key type for the bound MCP caller.
type mcpContextKey struct{}

// bindMcpCaller returns ctx carrying the caller (context values are
// immutable; the MCP transport binds per-request and lets it drop).
func bindMcpCaller(ctx context.Context, c *McpCaller) context.Context {
	if c == nil {
		return ctx
	}
	return context.WithValue(ctx, mcpContextKey{}, c)
}

// currentMcpCaller reads the bound MCP caller (nil when unbound).
func currentMcpCaller(ctx context.Context) *McpCaller {
	c, _ := ctx.Value(mcpContextKey{}).(*McpCaller)
	return c
}
