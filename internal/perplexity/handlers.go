package perplexity

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/clssck/researcher-mcp/internal/db"
	"github.com/clssck/researcher-mcp/pkg/mcp"
	"github.com/google/uuid"
)

// --- Handler Utilities ---

// Helper to unmarshal generic arguments into a specific struct
func unmarshalArguments(args map[string]interface{}, target interface{}) error {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("failed to marshal generic arguments: %w", err)
	}
	if err := json.Unmarshal(argsJSON, target); err != nil {
		return fmt.Errorf("failed to unmarshal arguments into target struct: %w", err)
	}
	return nil
}

// Helper to format text response for MCP
func formatTextResponse(text string) *mcp.CallToolResponse {
	return &mcp.CallToolResponse{
		Content: []mcp.CallToolResultContent{
			{
				Type: "text",
				Text: text,
			},
		},
	}
}

// --- Tool Handlers ---

// ChatPerplexityArgs defines the expected arguments for the chat_perplexity tool.
// Use pointers for optional fields.
type ChatPerplexityArgs struct {
	Message string  `json:"message"`
	ChatID  *string `json:"chat_id,omitempty"`
}

// HandleChatPerplexity manages conversations with the Perplexity API.
func HandleChatPerplexity(ctx context.Context, pplxClient *Client, dbClient *db.Database, args map[string]interface{}) (*mcp.CallToolResponse, error) {
	var toolArgs ChatPerplexityArgs
	if err := unmarshalArguments(args, &toolArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments for chat_perplexity: %w", err)
	}

	if toolArgs.Message == "" {
		return nil, fmt.Errorf("message argument cannot be empty")
	}

	// Determine Chat ID
	chatID := ""
	if toolArgs.ChatID != nil && *toolArgs.ChatID != "" {
		chatID = *toolArgs.ChatID
	} else {
		chatID = uuid.NewString()
		log.Printf("No chat_id provided, starting new chat with ID: %s", chatID)
	}

	// 1. Get existing chat history
	dbHistory, err := dbClient.GetChatHistory(chatID)
	if err != nil {
		// Don't fail hard if history fetch fails, could be a new chat. Log it.
		log.Printf("Warning: Failed to get chat history for %s: %v. Proceeding as new chat if applicable.", chatID, err)
		dbHistory = []db.ChatMessage{} // Ensure history is not nil
	}

	// 2. Save new user message
	userMessage := db.ChatMessage{
		ChatID:  chatID,
		Role:    "user",
		Content: toolArgs.Message,
		// CreatedAt is handled by DB default
	}
	if err := dbClient.SaveChatMessage(userMessage); err != nil {
		// Failing to save the user message is critical
		return nil, fmt.Errorf("failed to save user message for chat %s: %w", chatID, err)
	}

	// 3. Prepare messages for API call (Convert db.ChatMessage to perplexity.Message)
	apiMessages := make([]Message, 0, len(dbHistory)+1)
	for _, dbMsg := range dbHistory {
		apiMessages = append(apiMessages, Message{
			Role:    dbMsg.Role,
			Content: dbMsg.Content,
		})
	}
	// Add the new user message
	apiMessages = append(apiMessages, Message{
		Role:    userMessage.Role,
		Content: userMessage.Content,
	})

	// TODO: Potentially add logic here to ensure strict role alternation if needed,
	// although the API might handle this gracefully.

	// 4. Call Perplexity API
	apiResp, err := pplxClient.PostChatCompletions(ctx, apiMessages)
	if err != nil {
		return nil, fmt.Errorf("Perplexity API call failed: %w", err)
	}

	if len(apiResp.Choices) == 0 {
		return nil, fmt.Errorf("Perplexity API returned no choices")
	}
	assistantContent := apiResp.Choices[0].Message.Content

	// 5. Save assistant response to DB
	assistantMessage := db.ChatMessage{
		ChatID:  chatID,
		Role:    "assistant",
		Content: assistantContent,
		Tokens:  sql.NullInt64{Int64: int64(apiResp.Usage.CompletionTokens), Valid: true}, // Store completion tokens
		Model:   sql.NullString{String: apiResp.Model, Valid: apiResp.Model != ""},
		// Metadata: Can store full Usage struct as JSON if needed
		// metadataStr, _ := json.Marshal(apiResp.Usage)
		// Metadata: sql.NullString{String: string(metadataStr), Valid: true},
	}
	if err := dbClient.SaveChatMessage(assistantMessage); err != nil {
		// Log the error but still return the response to the user
		log.Printf("Warning: Failed to save assistant message for chat %s: %v", chatID, err)
	}

	// 6. Format and return response
	log.Printf("Successfully handled chat_perplexity for chat ID %s", chatID)
	return formatTextResponse(assistantContent), nil
}

// --- Placeholder Handlers for other tools ---

// TODO: Implement HandleSearch
// TODO: Implement HandleGetDocumentation
// TODO: Implement HandleFindApis
// TODO: Implement HandleCheckDeprecatedCode
// TODO: Implement HandleGetRequestStatus
