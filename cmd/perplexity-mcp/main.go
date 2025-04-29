package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/clssck/researcher-mcp/internal/db"
	"github.com/clssck/researcher-mcp/internal/perplexity"
	"github.com/clssck/researcher-mcp/pkg/mcp"
	// "github.com/google/uuid" // Import only if needed directly in main, handlers.go uses it
)

// App holds application-wide dependencies
type App struct {
	dbClient   *db.Database
	pplxClient *perplexity.Client
}

func main() {
	log.SetOutput(os.Stderr)
	log.Println("Starting Perplexity MCP server...")

	// --- Configuration ---
	apiKey := os.Getenv("PERPLEXITY_API_KEY")
	if apiKey == "" {
		log.Fatal("Error: PERPLEXITY_API_KEY environment variable is not set.")
	}
	model := os.Getenv("PERPLEXITY_MODEL") // Optional, defaults handled in client

	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("Error getting home directory: %v", err)
	}
	dbPath := filepath.Join(homeDir, ".perplexity-mcp", "chat_history.db")

	// --- Initialize Dependencies ---
	dbCfg := db.Config{
		Path:    dbPath,
		Timeout: 5 * time.Second,
	}
	dbClient, err := db.NewDatabase(dbCfg)
	if err != nil {
		log.Fatalf("Error initializing database: %v", err)
	}
	defer func() {
		if err := dbClient.Close(); err != nil {
			log.Printf("Error closing database: %v", err)
		}
	}()

	pplxCfg := perplexity.Config{
		APIKey: apiKey,
		Model:  model,
	}
	pplxClient, err := perplexity.NewClient(pplxCfg)
	if err != nil {
		log.Fatalf("Error initializing Perplexity client: %v", err)
	}

	app := &App{
		dbClient:   dbClient,
		pplxClient: pplxClient,
	}

	// --- Run Server Loop ---
	log.Println("Initialization complete. Starting MCP request loop...")
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	ctx := context.Background()

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				log.Println("Stdin closed, exiting.")
				break
			}
			log.Printf("Error reading from stdin: %v", err)
			writeErrorResponse(writer, nil, mcp.ErrorCodeParseError, fmt.Sprintf("MCP read error: %v", err))
			break
		}

		log.Printf("Received raw data: %s", string(line))

		var req mcp.RequestMessage
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("Error unmarshalling JSON request: %v", err)
			writeErrorResponse(writer, nil, mcp.ErrorCodeParseError, fmt.Sprintf("Failed to parse JSON: %v", err))
			continue
		}

		log.Printf("Processing request ID %v, Method %s", req.ID, req.Method)

		if req.Jsonrpc != "2.0" {
			log.Printf("Invalid jsonrpc version: %s", req.Jsonrpc)
			writeErrorResponse(writer, req.ID, mcp.ErrorCodeInvalidRequest, "Invalid jsonrpc version, expected \"2.0\"")
			continue
		}

		var resp mcp.ResponseMessage
		switch req.Method {
		case "list_tools":
			resp = handleListTools(req) // Doesn't need app context
		case "call_tool":
			resp = app.handleCallTool(ctx, req) // Pass context and app dependencies
		default:
			log.Printf("Method not found: %s", req.Method)
			resp = mcp.ResponseMessage{
				Jsonrpc: "2.0",
				ID:      req.ID,
				Error: &mcp.Error{
					Code:    mcp.ErrorCodeMethodNotFound,
					Message: fmt.Sprintf("Method not found: %s", req.Method),
				},
			}
		}

		if err := writeResponse(writer, resp); err != nil {
			log.Printf("Error writing response: %v", err)
			break
		}
		writer.Flush()
	}

	log.Println("Perplexity MCP server stopped.")
}

// handleListTools generates the response for the "list_tools" request.
func handleListTools(req mcp.RequestMessage) mcp.ResponseMessage {
	tools := []mcp.Tool{
		{
			Name:        "get_request_status",
			Description: "Get the current status of a processing request",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"request_id": map[string]interface{}{
						"type":        "string",
						"description": "The ID of the request to check status for",
					},
				},
				"required": []string{"request_id"},
			},
		},
		{
			Name:        "chat_perplexity",
			Description: "Maintains ongoing conversations with Perplexity AI. Creates new chats or continues existing ones with full history context.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"message": map[string]interface{}{
						"type":        "string",
						"description": "The message to send to Perplexity AI",
					},
					"chat_id": map[string]interface{}{
						"type":        "string",
						"description": "Optional: ID of an existing chat to continue. If not provided, a new chat will be created.",
					},
				},
				"required": []string{"message"},
			},
		},
		{
			Name:        "search",
			Description: "Perform a general search query to get comprehensive information on any topic",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The search query or question",
					},
					"detail_level": map[string]interface{}{
						"type":        "string",
						"description": "Optional: Desired level of detail (brief, normal, detailed)",
						"enum":        []string{"brief", "normal", "detailed"},
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "get_documentation",
			Description: "Get documentation and usage examples for a specific technology, library, or API",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "The technology, library, or API to get documentation for",
					},
					"context": map[string]interface{}{
						"type":        "string",
						"description": "Additional context or specific aspects to focus on",
					},
				},
				"required": []string{"query"},
			},
		},
		{
			Name:        "find_apis",
			Description: "Find and evaluate APIs that could be integrated into a project",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"requirement": map[string]interface{}{
						"type":        "string",
						"description": "The functionality or requirement you're looking to fulfill",
					},
					"context": map[string]interface{}{
						"type":        "string",
						"description": "Additional context about the project or specific needs",
					},
				},
				"required": []string{"requirement"},
			},
		},
		{
			Name:        "check_deprecated_code",
			Description: "Check if code or dependencies might be using deprecated features",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"code": map[string]interface{}{
						"type":        "string",
						"description": "The code snippet or dependency to check",
					},
					"technology": map[string]interface{}{
						"type":        "string",
						"description": "The technology or framework context (e.g., 'React', 'Node.js')",
					},
				},
				"required": []string{"code"},
			},
		},
	}
	return mcp.ResponseMessage{
		Jsonrpc: "2.0",
		ID:      req.ID,
		Result: mcp.ListToolsResponse{
			Tools: tools,
		},
	}
}

// handleCallTool routes the "call_tool" request to the appropriate handler.
func (app *App) handleCallTool(ctx context.Context, req mcp.RequestMessage) mcp.ResponseMessage {
	paramsJSON, err := json.Marshal(req.Params)
	if err != nil {
		log.Printf("Error marshalling params for CallToolRequest: %v", err)
		return mcp.ResponseMessage{
			Jsonrpc: "2.0", ID: req.ID, Error: &mcp.Error{Code: mcp.ErrorCodeInvalidRequest, Message: "Invalid params structure"},
		}
	}

	var params mcp.CallToolParams
	if err := json.Unmarshal(paramsJSON, &params); err != nil {
		log.Printf("Error unmarshalling CallToolParams: %v", err)
		return mcp.ResponseMessage{
			Jsonrpc: "2.0", ID: req.ID, Error: &mcp.Error{Code: mcp.ErrorCodeInvalidParams, Message: fmt.Sprintf("Failed to parse tool parameters: %v", err)},
		}
	}

	log.Printf("Handling call_tool for tool: %s", params.Name)

	var result interface{}
	var handlerErr error

	switch params.Name {
	case "chat_perplexity":
		result, handlerErr = perplexity.HandleChatPerplexity(ctx, app.pplxClient, app.dbClient, params.Arguments)
	case "search":
		log.Println("search handler not implemented yet")
		handlerErr = fmt.Errorf("tool '%s' handler not implemented", params.Name)
	case "get_documentation":
		log.Println("get_documentation handler not implemented yet")
		handlerErr = fmt.Errorf("tool '%s' handler not implemented", params.Name)
	case "find_apis":
		log.Println("find_apis handler not implemented yet")
		handlerErr = fmt.Errorf("tool '%s' handler not implemented", params.Name)
	case "check_deprecated_code":
		log.Println("check_deprecated_code handler not implemented yet")
		handlerErr = fmt.Errorf("tool '%s' handler not implemented", params.Name)
	case "get_request_status":
		log.Println("get_request_status handler not implemented yet")
		handlerErr = fmt.Errorf("tool '%s' handler not implemented", params.Name)
	default:
		log.Printf("Unknown tool name: %s", params.Name)
		handlerErr = fmt.Errorf("unknown tool: %s", params.Name)
	}

	if handlerErr != nil {
		log.Printf("Error calling tool %s: %v", params.Name, handlerErr)
		errorCode := mcp.ErrorCodeInternalError
		// TODO: Map handlerErr types to specific MCP error codes if needed
		return mcp.ResponseMessage{
			Jsonrpc: "2.0", ID: req.ID, Error: &mcp.Error{Code: errorCode, Message: handlerErr.Error()},
		}
	}

	// If handler succeeded, the result should be marshalable (likely *mcp.CallToolResponse)
	return mcp.ResponseMessage{
		Jsonrpc: "2.0", ID: req.ID, Result: result,
	}
}

// writeResponse marshals the response message and writes it to the writer.
func writeResponse(writer *bufio.Writer, resp mcp.ResponseMessage) error {
	respBytes, err := json.Marshal(resp)
	if err != nil {
		log.Printf("Error marshalling response: %v", err)
		minimalErrResp := mcp.ResponseMessage{
			Jsonrpc: "2.0", ID: resp.ID,
			Error: &mcp.Error{Code: mcp.ErrorCodeInternalError, Message: "Failed to marshal response object"},
		}
		respBytes, _ = json.Marshal(minimalErrResp)
	}

	log.Printf("Sending response: %s", string(respBytes))
	_, err = writer.Write(respBytes)
	if err != nil {
		return fmt.Errorf("writing response failed: %w", err)
	}
	_, err = writer.Write([]byte("\n")) // Add newline delimiter
	if err != nil {
		return fmt.Errorf("writing newline failed: %w", err)
	}
	return nil
}

// writeErrorResponse is a helper to construct and write an error response.
func writeErrorResponse(writer *bufio.Writer, id interface{}, code mcp.ErrorCode, message string) {
	errResp := mcp.ResponseMessage{
		Jsonrpc: "2.0", ID: id, Error: &mcp.Error{Code: code, Message: message},
	}
	if err := writeResponse(writer, errResp); err != nil {
		log.Printf("Failed to write error response: %v", err)
	}
	writer.Flush() // Ensure error is sent immediately
}
