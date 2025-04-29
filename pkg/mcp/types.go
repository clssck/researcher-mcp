package mcp

// Based on MCP specification
// Ref: Check MCP SDK or spec docs for exact field names and types if available.

// ErrorCode defines the standard MCP error codes.
type ErrorCode int

const (
	ErrorCodeParseError     ErrorCode = -32700
	ErrorCodeInvalidRequest ErrorCode = -32600
	ErrorCodeMethodNotFound ErrorCode = -32601
	ErrorCodeInvalidParams  ErrorCode = -32602
	ErrorCodeInternalError  ErrorCode = -32603
	// Add other MCP-specific or server-specific codes as needed
)

// Error represents the error object in a JSON-RPC response.
type Error struct {
	Code    ErrorCode   `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// RequestMessage represents a generic MCP request.
type RequestMessage struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"` // Use interface{} and unmarshal later based on Method
	ID      interface{} `json:"id"`               // Can be string, number, or null
}

// ResponseMessage represents a generic MCP response.
type ResponseMessage struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   *Error      `json:"error,omitempty"`
	ID      interface{} `json:"id"` // Should match the request ID
}

// Tool represents the definition of a tool available on the server.
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"` // Using map for flexibility, consider struct if schema is fixed
}

// ListToolsResponse is the structure for the result of a "list_tools" request.
type ListToolsResponse struct {
	Tools []Tool `json:"tools"`
}

// CallToolParams represents the parameters for a "call_tool" request.
// We use map[string]interface{} for arguments initially for flexibility.
type CallToolParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

// CallToolResultContent represents a single piece of content in the result.
type CallToolResultContent struct {
	Type string `json:"type"` // e.g., "text"
	Text string `json:"text,omitempty"`
}

// CallToolResponse is the structure for the result of a "call_tool" request.
type CallToolResponse struct {
	Content []CallToolResultContent `json:"content"`
}
