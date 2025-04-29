package db

import (
	"database/sql" // Needed for metadata if stored as JSON text
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3" // SQLite driver
)

// --- Structs ---

// ChatMessage corresponds to the structure in the 'messages' table.
type ChatMessage struct {
	ID        int64          `json:"-"` // Internal DB ID
	ChatID    string         `json:"chat_id"`
	Role      string         `json:"role"`      // 'user', 'assistant', 'system'
	Content   string         `json:"content"`
	CreatedAt time.Time      `json:"created_at"`
	Tokens    sql.NullInt64  `json:"tokens,omitempty"`    // Use sql.NullInt64 for nullable integer
	Model     sql.NullString `json:"model,omitempty"`     // Use sql.NullString for nullable text
	Metadata  sql.NullString `json:"metadata,omitempty"` // Store as JSON string, use sql.NullString
}

// RequestStatus corresponds to the structure in the 'request_status' table.
type RequestStatus struct {
	ID                  string       `json:"id"`
	ChatID              string       `json:"chat_id"`
	Status              string       `json:"status"` // 'queued', 'processing', 'complete'
	Progress            sql.NullInt64  `json:"progress,omitempty"`
	CurrentStage        sql.NullString `json:"current_stage,omitempty"`
	EstimatedCompletion sql.NullTime   `json:"estimated_completion,omitempty"` // Use sql.NullTime for nullable timestamp
	CreatedAt           time.Time    `json:"created_at"`
}

// --- Config and Database Setup ---

// Config holds database configuration.
type Config struct {
	Path    string
	Timeout time.Duration
}

// Database wraps the sql.DB connection.
type Database struct {
	*sql.DB
}

// NewDatabase creates a new database connection and ensures the schema is initialized.
func NewDatabase(cfg Config) (*Database, error) {
	dbDir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create database directory %s: %w", dbDir, err)
	}

	// Add timeout to DSN
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_busy_timeout=%d", cfg.Path, cfg.Timeout.Milliseconds())
	log.Printf("Connecting to database: %s", cfg.Path) // Don't log DSN with timeout

	dbConn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database %s: %w", cfg.Path, err)
	}

	// Set connection pool settings (adjust as needed)
	dbConn.SetMaxOpenConns(10) // Limit concurrent connections
	dbConn.SetMaxIdleConns(5)
	dbConn.SetConnMaxLifetime(5 * time.Minute)

	db := &Database{DB: dbConn}

	if err := db.initializeSchema(); err != nil {
		_ = db.Close() // Attempt to close connection if initialization fails
		return nil, fmt.Errorf("failed to initialize database schema: %w", err)
	}

	log.Println("Database connection established and schema initialized.")
	return db, nil
}

// initializeSchema creates or migrates the necessary tables and indexes.
func (db *Database) initializeSchema() error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	// Use a named return for the error to ensure Rollback happens on error return
	var txErr error
	defer func() {
		if txErr != nil {
			log.Printf("Rolling back schema transaction due to error: %v", txErr)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				log.Printf("Error during transaction rollback: %v", rollbackErr)
			}
		}
	}()

	// Enable foreign keys
	_, txErr = tx.Exec("PRAGMA foreign_keys = ON;")
	if txErr != nil {
		txErr = fmt.Errorf("failed to enable foreign keys: %w", txErr)
		return txErr
	}

	// --- Chats Table ---
	_, txErr = tx.Exec(`
	CREATE TABLE IF NOT EXISTS chats (
		id TEXT PRIMARY KEY,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL,
		last_accessed DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL,
		title TEXT DEFAULT '',
		metadata TEXT DEFAULT '{}',
		user_id TEXT DEFAULT '',
		tags TEXT DEFAULT '[]'
	);
	`)
	if txErr != nil {
		txErr = fmt.Errorf("failed to create chats table: %w", txErr)
		return txErr
	}

	// Add columns to chats table if they don't exist (basic migration)
	_ = addColumnIfNotExists(tx, "chats", "last_accessed", "DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL")
	_ = addColumnIfNotExists(tx, "chats", "title", "TEXT DEFAULT ''")
	_ = addColumnIfNotExists(tx, "chats", "metadata", "TEXT DEFAULT '{}'")
	_ = addColumnIfNotExists(tx, "chats", "user_id", "TEXT DEFAULT ''")
	_ = addColumnIfNotExists(tx, "chats", "tags", "TEXT DEFAULT '[]'")

	// --- Messages Table ---
	_, txErr = tx.Exec(`
	CREATE TABLE IF NOT EXISTS messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_id TEXT NOT NULL,
		role TEXT CHECK(role IN ('user', 'assistant', 'system')) NOT NULL,
		content TEXT NOT NULL,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL,
		tokens INTEGER DEFAULT 0,
		model TEXT DEFAULT '',
		metadata TEXT DEFAULT '{}',
		FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
	);
	`)
	if txErr != nil {
		txErr = fmt.Errorf("failed to create messages table: %w", txErr)
		return txErr
	}

	// Add columns to messages table if they don't exist
	_ = addColumnIfNotExists(tx, "messages", "tokens", "INTEGER DEFAULT 0")
	_ = addColumnIfNotExists(tx, "messages", "model", "TEXT DEFAULT ''")
	_ = addColumnIfNotExists(tx, "messages", "metadata", "TEXT DEFAULT '{}'")

	// --- Request Status Table ---
	_, txErr = tx.Exec(`
	CREATE TABLE IF NOT EXISTS request_status (
		id TEXT PRIMARY KEY,
		chat_id TEXT NOT NULL,
		status TEXT CHECK(status IN ('queued', 'processing', 'complete')) NOT NULL,
		progress INTEGER DEFAULT 0,
		current_stage TEXT,
		estimated_completion DATETIME,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL,
		FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
	);
	`)
	if txErr != nil {
		txErr = fmt.Errorf("failed to create request_status table: %w", txErr)
		return txErr
	}

	// --- Indexes ---
	indexQueries := []string{
		"CREATE INDEX IF NOT EXISTS idx_chats_last_accessed ON chats(last_accessed);",
		"CREATE INDEX IF NOT EXISTS idx_chats_user_id ON chats(user_id);",
		"CREATE INDEX IF NOT EXISTS idx_messages_chat_id ON messages(chat_id);",
		"CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at);",
		"CREATE INDEX IF NOT EXISTS idx_messages_chat_role ON messages(chat_id, role);",
		"CREATE INDEX IF NOT EXISTS idx_messages_tokens ON messages(tokens);",
		"CREATE INDEX IF NOT EXISTS idx_request_status_chat_id ON request_status(chat_id);",
		"CREATE INDEX IF NOT EXISTS idx_request_status_status ON request_status(status);",
	}

	for _, query := range indexQueries {
		if _, txErr = tx.Exec(query); txErr != nil {
			log.Printf("Warning: Failed to create index (%s): %v", query, txErr)
			// Don't return error on index failure, just log it.
			txErr = nil // Reset error so we don't rollback unnecessarily
		}
	}

	// Commit the transaction
	txErr = tx.Commit()
	if txErr != nil {
		txErr = fmt.Errorf("failed to commit schema initialization transaction: %w", txErr)
		return txErr
	}

	return nil // Success
}

// Helper function to add a column if it doesn't exist
func addColumnIfNotExists(tx *sql.Tx, tableName, columnName, columnDefinition string) error {
	rows, err := tx.Query(fmt.Sprintf("PRAGMA table_info(%s)", tableName))
	if err != nil {
		log.Printf("Warning: Could not query table info for %s: %v", tableName, err)
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name string
		var type_ string // 'type' is a keyword
		var notnull int
		var dflt_value interface{}
		var pk int
		if err := rows.Scan(&cid, &name, &type_, &notnull, &dflt_value, &pk); err != nil {
			log.Printf("Warning: Could not scan column info for %s: %v", tableName, err)
			return err // Return error here as scan failure is significant
		}
		if name == columnName {
			return nil // Column exists
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("Warning: Error iterating rows for table info %s: %v", tableName, err)
		return err
	}

	// Column does not exist, add it
	alterQuery := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, columnDefinition)
	log.Printf("Adding column: %s", alterQuery)
	_, err = tx.Exec(alterQuery)
	if err != nil {
		log.Printf("Warning: Failed to add column %s to table %s: %v", columnName, tableName, err)
		return err
	}
	return nil
}

// --- Data Access Methods ---

// GetChatHistory retrieves all messages for a given chat ID, ordered by creation time.
// It also updates the last_accessed time for the chat.
func (db *Database) GetChatHistory(chatID string) ([]ChatMessage, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction for getting chat history: %w", err)
	}
	var txErr error
	defer func() {
		if txErr != nil {
			log.Printf("Rolling back GetChatHistory transaction due to error: %v", txErr)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				log.Printf("Error during GetChatHistory transaction rollback: %v", rollbackErr)
			}
		}
	}()

	// Update last accessed time
	_, txErr = tx.Exec("UPDATE chats SET last_accessed = CURRENT_TIMESTAMP WHERE id = ?", chatID)
	if txErr != nil && txErr != sql.ErrNoRows { // Ignore error if chat doesn't exist yet
		log.Printf("Warning: Failed to update last_accessed for chat %s: %v", chatID, txErr)
		// Don't return, proceed to query messages (history might be empty)
		txErr = nil // Reset error
	} else if txErr == sql.ErrNoRows {
		txErr = nil // Reset error if chat just doesn't exist
	}


	query := `SELECT id, chat_id, role, content, created_at, tokens, model, metadata
			 FROM messages
			 WHERE chat_id = ?
			 ORDER BY created_at ASC`
	rows, txErr := tx.Query(query, chatID)
	if txErr != nil {
		txErr = fmt.Errorf("failed to query chat history for chat %s: %w", chatID, txErr)
		return nil, txErr
	}
	defer rows.Close()

	var history []ChatMessage
	for rows.Next() {
		var msg ChatMessage
		if txErr = rows.Scan(
			&msg.ID,
			&msg.ChatID,
			&msg.Role,
			&msg.Content,
			&msg.CreatedAt,
			&msg.Tokens,
			&msg.Model,
			&msg.Metadata,
		); txErr != nil {
			txErr = fmt.Errorf("failed to scan chat message row: %w", txErr)
			return nil, txErr
		}
		history = append(history, msg)
	}

	if txErr = rows.Err(); txErr != nil {
		txErr = fmt.Errorf("error during chat history iteration: %w", txErr)
		return nil, txErr
	}

	// Commit the transaction
	txErr = tx.Commit()
	if txErr != nil {
		txErr = fmt.Errorf("failed to commit transaction for getting chat history: %w", txErr)
		return nil, txErr
	}

	return history, nil
}

// SaveChatMessage saves a new message to the database.
// It ensures the chat exists before inserting the message.
func (db *Database) SaveChatMessage(msg ChatMessage) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("failed to begin transaction for saving chat message: %w", err)
	}
	var txErr error
	defer func() {
		if txErr != nil {
			log.Printf("Rolling back SaveChatMessage transaction due to error: %v", txErr)
			if rollbackErr := tx.Rollback(); rollbackErr != nil {
				log.Printf("Error during SaveChatMessage transaction rollback: %v", rollbackErr)
			}
		}
	}()

	// Ensure chat exists
	_, txErr = tx.Exec("INSERT OR IGNORE INTO chats (id) VALUES (?)", msg.ChatID)
	if txErr != nil {
		txErr = fmt.Errorf("failed to ensure chat %s exists: %w", msg.ChatID, txErr)
		return txErr
	}

	// Save message
	query := `INSERT INTO messages (chat_id, role, content, tokens, model, metadata)
			 VALUES (?, ?, ?, ?, ?, ?)`
	_, txErr = tx.Exec(query, msg.ChatID, msg.Role, msg.Content, msg.Tokens, msg.Model, msg.Metadata)
	if txErr != nil {
		txErr = fmt.Errorf("failed to insert chat message for chat %s: %w", msg.ChatID, txErr)
		return txErr
	}

	// Commit the transaction
	txErr = tx.Commit()
	if txErr != nil {
		txErr = fmt.Errorf("failed to commit transaction for saving chat message: %w", txErr)
		return txErr
	}

	return nil
}

// GetRequestStatus retrieves the status of a specific request by its ID.
func (db *Database) GetRequestStatus(requestID string) (*RequestStatus, error) {
	query := `SELECT id, chat_id, status, progress, current_stage, estimated_completion, created_at
			 FROM request_status
			 WHERE id = ?`
	row := db.QueryRow(query, requestID)

	var status RequestStatus
	err := row.Scan(
		&status.ID,
		&status.ChatID,
		&status.Status,
		&status.Progress,
		&status.CurrentStage,
		&status.EstimatedCompletion,
		&status.CreatedAt,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil // Not found is not an error in this context
		}
		return nil, fmt.Errorf("failed to scan request status for ID %s: %w", requestID, err)
	}

	return &status, nil
}

// SaveRequestStatus inserts or updates a request status record.
func (db *Database) SaveRequestStatus(status RequestStatus) error {
	// Use INSERT ... ON CONFLICT DO UPDATE for atomic upsert if supported and desired,
	// or separate INSERT and UPDATE. INSERT OR REPLACE is simpler but less flexible.
	query := `INSERT OR REPLACE INTO request_status
			 (id, chat_id, status, progress, current_stage, estimated_completion, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`

	// Handle default created_at time only if it's zero
	createdAt := status.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}

	_, err := db.Exec(query,
		status.ID,
		status.ChatID,
		status.Status,
		status.Progress,
		status.CurrentStage,
		status.EstimatedCompletion,
		createdAt,
	)

	if err != nil {
		return fmt.Errorf("failed to save request status for ID %s: %w", status.ID, err)
	}

	return nil
}
