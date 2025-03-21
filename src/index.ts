#!/usr/bin/env node

import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ErrorCode,
  ListToolsRequestSchema,
  McpError,
} from '@modelcontextprotocol/sdk/types.js';
import { HttpsAgent } from 'agentkeepalive';
import axios, { AxiosError, AxiosInstance } from 'axios';
import axiosRetry, { isNetworkError } from 'axios-retry';
import Database from 'better-sqlite3';
import { existsSync, mkdirSync } from 'fs';
import { homedir } from 'os';
import { dirname, join } from 'path';

const PERPLEXITY_API_KEY = process.env.PERPLEXITY_API_KEY;
if (!PERPLEXITY_API_KEY) {
  throw new Error('PERPLEXITY_API_KEY environment variable is required');
}

interface ChatMessage {
  role: 'user' | 'assistant';
  content: string;
  created_at?: Date;
  tokens?: number;
  model?: string;
  metadata?: string;
}

interface ServerConfig {
  apiKey: string;
  baseURL: string;
  rateLimit: {
    maxRequests: number;
    windowSize: number;
  };
  timeout: number;
  dbPath: string;
  model?: string;
}

interface ColumnInfo {
  cid: number;
  name: string;
  type: string;
  notnull: number;
  dflt_value: any;
  pk: number;
}

class PerplexityServer {
  private server!: Server;
  private axiosInstance!: AxiosInstance;
  private db!: Database.Database;
  private config: ServerConfig;

  private isColumnInfo(obj: unknown): obj is ColumnInfo {
    return (
      typeof obj === 'object' &&
      obj !== null &&
      'cid' in obj &&
      'name' in obj &&
      'type' in obj &&
      'notnull' in obj &&
      'pk' in obj
    );
  }

  constructor(config: ServerConfig) {
    this.config = config;

    this.server = new Server(
      {
        name: 'perplexity-server',
        version: '0.1.0',
      },
      {
        capabilities: {
          tools: {},
        },
      },
    );

    // Configure connection pooling
    const httpsAgent = new HttpsAgent({
      maxSockets: 100,
      maxFreeSockets: 10,
      timeout: this.config.timeout,
      freeSocketTimeout: 30000,
    });

    this.axiosInstance = axios.create({
      baseURL: this.config.baseURL,
      headers: {
        Authorization: `Bearer ${this.config.apiKey}`,
        'Content-Type': 'application/json',
      },
      timeout: this.config.timeout,
      httpsAgent: httpsAgent,
      validateStatus: (status) => status >= 200 && status < 500, // Don't reject on 4xx errors
    });

    // Configure retry logic with more attempts and longer delays
    axiosRetry(this.axiosInstance, {
      retries: 5, // Increased from 3
      retryDelay: (retryCount) => {
        const delay = Math.min(1000 * Math.pow(2, retryCount), 10000);
        return delay;
      },
      retryCondition: (error: AxiosError) => {
        return Boolean(
          isNetworkError(error) ||
            error.code === 'ECONNABORTED' ||
            (error.response?.status &&
              [408, 429, 500, 502, 503, 504].includes(error.response.status)),
        );
      },
      onRetry: (retryCount: number, error: AxiosError<unknown>) => {
        console.warn(
          `Retry attempt ${retryCount} for ${error.config?.url}: ${error.message}`,
        );
      },
    });

    // Add request interceptor for better error handling
    this.axiosInstance.interceptors.request.use(
      (config) => {
        console.log(`Making request to ${config.url}`);
        return config;
      },
      (error) => {
        console.error('Request error:', error.message);
        return Promise.reject(error);
      },
    );

    // Add response interceptor for better error handling
    this.axiosInstance.interceptors.response.use(
      (response) => {
        console.log(`Response from ${response.config.url}: ${response.status}`);
        return response;
      },
      (error) => {
        if (error.response) {
          console.error(
            `API Error (${error.response.status}):`,
            error.response.data,
          );
        } else if (error.request) {
          console.error('No response received:', error.message);
        } else {
          console.error('Request setup error:', error.message);
        }
        return Promise.reject(error);
      },
    );

    // Initialize SQLite database
    const dbDir = dirname(this.config.dbPath);
    if (!existsSync(dbDir)) {
      mkdirSync(dbDir, { recursive: true });
    }

    // Configure connection pooling and performance settings
    this.db = new Database(this.config.dbPath, {
      fileMustExist: false,
      verbose: console.log,
      timeout: 5000, // Wait up to 5s if DB is locked
    });

    // Set practical limits for concurrent access
    this.db.pragma('journal_mode = WAL');
    this.db.pragma('cache_size = -10000'); // 10MB cache
    this.db.pragma('foreign_keys = ON');

    // Set practical limits for concurrent access
    this.db.pragma('journal_mode = WAL');
    this.db.pragma('cache_size = -10000'); // 10MB cache
    this.db.pragma('foreign_keys = ON');
    this.initializeDatabase();

    this.setupToolHandlers();

    // Error handling
    this.server.onerror = (error) => console.error('[MCP Error]', error);
    process.on('SIGINT', async () => {
      this.db.close();
      await this.server.close();
      process.exit(0);
    });

    // Add rate limit tracking with improved handling
    const rateLimits = {
      lastRequestTime: Date.now(),
      requestCount: 0,
      maxRequests: this.config.rateLimit.maxRequests,
      windowSize: this.config.rateLimit.windowSize,
      retryAfter: 0,
    };

    // Add rate limit interceptor with exponential backoff
    this.axiosInstance.interceptors.request.use(async (config) => {
      const now = Date.now();
      const windowElapsed = now - rateLimits.lastRequestTime;

      // Reset counter if window has elapsed
      if (windowElapsed >= rateLimits.windowSize) {
        rateLimits.requestCount = 0;
        rateLimits.lastRequestTime = now;
        rateLimits.retryAfter = 0;
      }

      // If we're over the limit, wait with exponential backoff
      if (rateLimits.requestCount >= rateLimits.maxRequests) {
        const baseDelay = rateLimits.windowSize - windowElapsed;
        const backoffFactor = Math.floor(
          rateLimits.requestCount / rateLimits.maxRequests,
        );
        const delay = Math.min(baseDelay * Math.pow(2, backoffFactor), 30000); // Max 30s delay

        console.warn(`Rate limit reached. Waiting ${delay}ms before retry...`);
        await new Promise((resolve) => setTimeout(resolve, delay));

        // Recursively retry the request after delay
        return this.axiosInstance.request(config);
      }

      rateLimits.requestCount++;
      return config;
    });

    // Handle rate limit responses
    this.axiosInstance.interceptors.response.use(
      (response) => response,
      async (error) => {
        if (error.response?.status === 429) {
          const retryAfter =
            parseInt(error.response.headers['retry-after']) || 60;
          console.warn(
            `Rate limited by server. Waiting ${retryAfter}s before retry...`,
          );

          await new Promise((resolve) =>
            setTimeout(resolve, retryAfter * 1000),
          );
          return this.axiosInstance.request(error.config);
        }
        return Promise.reject(error);
      },
    );
  }

  private initializeDatabase() {
    try {
      this.db.exec('BEGIN TRANSACTION');

      // Create status tracking table
      this.db.exec(`
          CREATE TABLE IF NOT EXISTS request_status (
            id TEXT PRIMARY KEY,
            chat_id TEXT NOT NULL,
            status TEXT CHECK(status IN ('queued', 'processing', 'complete')) NOT NULL,
            progress INTEGER DEFAULT 0,
            current_stage TEXT,
            estimated_completion DATETIME,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
            FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
          )
        `);

      // Check if chats table exists
      const chatsTableExists = this.db
        .prepare(
          "SELECT name FROM sqlite_master WHERE type='table' AND name='chats'",
        )
        .get();

      if (!chatsTableExists) {
        // Create new chats table with all columns
        this.db.exec(`
      CREATE TABLE chats (
        id TEXT PRIMARY KEY,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
        last_accessed DATETIME DEFAULT CURRENT_TIMESTAMP,
        title TEXT DEFAULT '',
        metadata TEXT DEFAULT '{}',
        user_id TEXT DEFAULT '',
        tags TEXT DEFAULT '[]'
      )
    `);
      } else {
        // Add missing columns to existing chats table
        const columns = (
          this.db.prepare('PRAGMA table_info(chats)').all() as unknown[]
        )
          .filter(this.isColumnInfo)
          .map((col) => col.name);

        if (!columns.includes('last_accessed')) {
          this.db.exec('ALTER TABLE chats ADD COLUMN last_accessed DATETIME');
          // Initialize the new column with current timestamp
          this.db.exec('UPDATE chats SET last_accessed = CURRENT_TIMESTAMP');
        }
        if (!columns.includes('title')) {
          this.db.exec("ALTER TABLE chats ADD COLUMN title TEXT DEFAULT ''");
        }
        if (!columns.includes('metadata')) {
          this.db.exec('ALTER TABLE chats ADD COLUMN metadata TEXT');
          // Initialize the new column with empty object
          this.db.exec("UPDATE chats SET metadata = '{}'");
        }
        if (!columns.includes('user_id')) {
          this.db.exec("ALTER TABLE chats ADD COLUMN user_id TEXT DEFAULT ''");
        }
      }

      // Check if messages table exists
      const messagesTableExists = this.db
        .prepare(
          "SELECT name FROM sqlite_master WHERE type='table' AND name='messages'",
        )
        .get();

      if (!messagesTableExists) {
        // Create new messages table with all columns
        this.db.exec(`
      CREATE TABLE messages (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
        chat_id TEXT NOT NULL,
        role TEXT CHECK(role IN ('user', 'assistant', 'system')) NOT NULL,
        content TEXT NOT NULL,
        created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
        tokens INTEGER DEFAULT 0,
        model TEXT DEFAULT '',
        metadata TEXT DEFAULT '{}',
        FOREIGN KEY (chat_id) REFERENCES chats(id) ON DELETE CASCADE
      )
    `);
      } else {
        // Add missing columns to existing messages table
        const columns = (
          this.db.prepare('PRAGMA table_info(messages)').all() as unknown[]
        )
          .filter(this.isColumnInfo)
          .map((col) => col.name);

        if (!columns.includes('tokens')) {
          this.db.exec(
            'ALTER TABLE messages ADD COLUMN tokens INTEGER DEFAULT 0',
          );
        }
        if (!columns.includes('model')) {
          this.db.exec("ALTER TABLE messages ADD COLUMN model TEXT DEFAULT ''");
        }
        if (!columns.includes('metadata')) {
          this.db.exec(
            "ALTER TABLE messages ADD COLUMN metadata TEXT DEFAULT '{}'",
          );
        }
      }

      // Create indexes if they don't exist
      // Create indexes for frequently queried columns
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_chats_last_accessed ON chats(last_accessed)',
      );
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_messages_chat_id ON messages(chat_id)',
      );
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_messages_created_at ON messages(created_at)',
      );
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_chats_user_id ON chats(user_id)',
      );
      // Add composite index for common message queries
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_messages_chat_role ON messages(chat_id, role)',
      );
      // Add index for message tokens
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_messages_tokens ON messages(tokens)',
      );
      // Add index for chat metadata
      this.db.exec(
        'CREATE INDEX IF NOT EXISTS idx_chats_metadata ON chats(metadata)',
      );

      this.db.exec('COMMIT');
    } catch (error) {
      this.db.exec('ROLLBACK');
      throw new McpError(
        ErrorCode.InternalError,
        `Database initialization failed: ${error}`,
      );
    }
  }

  private getChatHistory(chatId: string): ChatMessage[] {
    try {
      // Update last accessed time
      this.db
        .prepare(
          'UPDATE chats SET last_accessed = CURRENT_TIMESTAMP WHERE id = ?',
        )
        .run(chatId);

      const stmt = this.db.prepare(
        'SELECT role, content, created_at, tokens, model, metadata FROM messages WHERE chat_id = ? ORDER BY created_at ASC',
      );
      const messages = stmt.all(chatId) as ChatMessage[];

      // Validate message structure
      return messages.map((msg) => ({
        role: msg.role,
        content: msg.content,
        ...(msg.created_at && { created_at: msg.created_at }),
        ...(msg.tokens && { tokens: msg.tokens }),
        ...(msg.model && { model: msg.model }),
        ...(msg.metadata && { metadata: msg.metadata }),
      }));
    } catch (error) {
      throw new McpError(
        ErrorCode.InternalError,
        `Failed to get chat history: ${error}`,
      );
    }
  }

  private saveChatMessage(chatId: string, message: ChatMessage) {
    try {
      this.db.exec('BEGIN TRANSACTION');

      // Ensure chat exists
      this.db
        .prepare('INSERT OR IGNORE INTO chats (id) VALUES (?)')
        .run(chatId);

      // Save message with additional metadata
      this.db
        .prepare(
          'INSERT INTO messages (chat_id, role, content, tokens, model, metadata) VALUES (?, ?, ?, ?, ?, ?)',
        )
        .run(
          chatId,
          message.role,
          message.content,
          message.tokens || 0,
          message.model || '',
          message.metadata || '{}',
        );

      this.db.exec('COMMIT');
    } catch (error) {
      this.db.exec('ROLLBACK');
      throw new McpError(
        ErrorCode.InternalError,
        `Failed to save chat message: ${error}`,
      );
    }
  }

  private setupToolHandlers() {
    this.server.setRequestHandler(ListToolsRequestSchema, async () => ({
      tools: [
        {
          name: 'get_request_status',
          description: 'Get the current status of a processing request',
          inputSchema: {
            type: 'object',
            properties: {
              request_id: {
                type: 'string',
                description: 'The ID of the request to check status for',
              },
            },
            required: ['request_id'],
          },
        },
        {
          name: 'chat_perplexity',
          description:
            'Maintains ongoing conversations with Perplexity AI. Creates new chats or continues existing ones with full history context.',
          inputSchema: {
            type: 'object',
            properties: {
              message: {
                type: 'string',
                description: 'The message to send to Perplexity AI',
              },
              chat_id: {
                type: 'string',
                description:
                  'Optional: ID of an existing chat to continue. If not provided, a new chat will be created.',
              },
            },
            required: ['message'],
          },
        },
        {
          name: 'search',
          description:
            'Perform a general search query to get comprehensive information on any topic',
          inputSchema: {
            type: 'object',
            properties: {
              query: {
                type: 'string',
                description: 'The search query or question',
              },
              detail_level: {
                type: 'string',
                description:
                  'Optional: Desired level of detail (brief, normal, detailed)',
                enum: ['brief', 'normal', 'detailed'],
              },
            },
            required: ['query'],
          },
        },
        {
          name: 'get_documentation',
          description:
            'Get documentation and usage examples for a specific technology, library, or API',
          inputSchema: {
            type: 'object',
            properties: {
              query: {
                type: 'string',
                description:
                  'The technology, library, or API to get documentation for',
              },
              context: {
                type: 'string',
                description:
                  'Additional context or specific aspects to focus on',
              },
            },
            required: ['query'],
          },
        },
        {
          name: 'find_apis',
          description:
            'Find and evaluate APIs that could be integrated into a project',
          inputSchema: {
            type: 'object',
            properties: {
              requirement: {
                type: 'string',
                description:
                  "The functionality or requirement you're looking to fulfill",
              },
              context: {
                type: 'string',
                description:
                  'Additional context about the project or specific needs',
              },
            },
            required: ['requirement'],
          },
        },
        {
          name: 'check_deprecated_code',
          description:
            'Check if code or dependencies might be using deprecated features',
          inputSchema: {
            type: 'object',
            properties: {
              code: {
                type: 'string',
                description: 'The code snippet or dependency to check',
              },
              technology: {
                type: 'string',
                description:
                  "The technology or framework context (e.g., 'React', 'Node.js')",
              },
            },
            required: ['code'],
          },
        },
      ],
    }));

    this.server.setRequestHandler(CallToolRequestSchema, async (request) => {
      try {
        switch (request.params.name) {
          case 'chat_perplexity': {
            const { message, chat_id = crypto.randomUUID() } = request.params
              .arguments as {
              message: string;
              chat_id?: string;
            };

            // Get chat history
            const history = this.getChatHistory(chat_id);

            // Add new user message
            const userMessage: ChatMessage = { role: 'user', content: message };
            this.saveChatMessage(chat_id, userMessage);

            // Prepare messages array with history, ensuring strict role alternation
            let messages = [...history];

            // Remove any consecutive user messages
            for (let i = messages.length - 1; i >= 0; i--) {
              if (messages[i].role === 'user') {
                messages.splice(i, 1);
              } else {
                break;
              }
            }

            // Add new user message
            messages.push(userMessage);

            // Call Perplexity API and wait for complete response
            const response = await this.axiosInstance.post(
              '/chat/completions',
              {
                model: this.config.model || 'sonar-reasoning-pro',
                messages,
                stream: false, // Explicitly disable streaming
              },
            );

            const assistantMessage: ChatMessage = {
              role: 'assistant',
              content: response.data.choices[0].message.content,
            };
            this.saveChatMessage(chat_id, assistantMessage);

            const responseText = response.data.choices[0].message.content;

            // Return the cleaned response
            return {
              content: [
                {
                  type: 'text',
                  text: responseText,
                },
              ],
            };
          }

          case 'get_documentation': {
            const { query, context = '' } = request.params.arguments as {
              query: string;
              context?: string;
            };
            const prompt = `Provide comprehensive documentation and usage examples for ${query}. ${
              context ? `Focus on: ${context}` : ''
            } Include:
            1. Basic overview and purpose
            2. Key features and capabilities
            3. Installation/setup if applicable
            4. Common usage examples
            5. Best practices
            6. Common pitfalls to avoid
            7. Links to official documentation if available`;

            const response = await this.axiosInstance.post(
              '/chat/completions',
              {
                model: this.config.model || 'sonar-reasoning-pro',
                messages: [{ role: 'user', content: prompt }],
                stream: false, // Explicitly disable streaming
              },
            );

            return {
              content: [
                {
                  type: 'text',
                  text: response.data.choices[0].message.content,
                },
              ],
            };
          }

          case 'find_apis': {
            const { requirement, context = '' } = request.params.arguments as {
              requirement: string;
              context?: string;
            };
            const prompt = `Find and evaluate APIs that could be used for: ${requirement}. ${
              context ? `Context: ${context}` : ''
            } For each API, provide:
            1. Name and brief description
            2. Key features and capabilities
            3. Pricing model (if available)
            4. Integration complexity
            5. Documentation quality
            6. Community support and popularity
            7. Any potential limitations or concerns
            8. Code example of basic usage`;

            const response = await this.axiosInstance.post(
              '/chat/completions',
              {
                model: this.config.model || 'sonar-reasoning-pro',
                messages: [{ role: 'user', content: prompt }],
                stream: false, // Explicitly disable streaming
              },
            );

            return {
              content: [
                {
                  type: 'text',
                  text: response.data.choices[0].message.content,
                },
              ],
            };
          }

          case 'check_deprecated_code': {
            const { code, technology = '' } = request.params.arguments as {
              code: string;
              technology?: string;
            };
            const prompt = `Analyze this code for deprecated features or patterns${
              technology ? ` in ${technology}` : ''
            }:

            ${code}

            Please provide:
            1. Identification of any deprecated features, methods, or patterns
            2. Current recommended alternatives
            3. Migration steps if applicable
            4. Impact of the deprecation
            5. Timeline of deprecation if known
            6. Code examples showing how to update to current best practices`;

            const response = await this.axiosInstance.post(
              '/chat/completions',
              {
                model: this.config.model || 'sonar-reasoning-pro',
                messages: [{ role: 'user', content: prompt }],
                stream: false, // Explicitly disable streaming
              },
            );

            return {
              content: [
                {
                  type: 'text',
                  text: response.data.choices[0].message.content,
                },
              ],
            };
          }

          case 'search': {
            const { query, detail_level = 'normal' } = request.params
              .arguments as {
              query: string;
              detail_level?: 'brief' | 'normal' | 'detailed';
            };

            let prompt = query;
            switch (detail_level) {
              case 'brief':
                prompt = `Provide a brief, concise answer to: ${query}`;
                break;
              case 'detailed':
                prompt = `Provide a comprehensive, detailed analysis of: ${query}. Include relevant examples, context, and supporting information where applicable.`;
                break;
              default:
                prompt = `Provide a clear, balanced answer to: ${query}. Include key points and relevant context.`;
            }

            const response = await this.axiosInstance.post(
              '/chat/completions',
              {
                model: this.config.model || 'sonar-reasoning-pro',
                messages: [{ role: 'user', content: prompt }],
                stream: false, // Explicitly disable streaming
              },
            );

            return {
              content: [
                {
                  type: 'text',
                  text: response.data.choices[0].message.content,
                },
              ],
            };
          }

          case 'get_request_status': {
            const { request_id } = request.params.arguments as {
              request_id: string;
            };

            const status = this.db
              .prepare(
                'SELECT status, progress, current_stage, estimated_completion FROM request_status WHERE id = ?',
              )
              .get(request_id);

            if (!status) {
              throw new McpError(
                ErrorCode.InternalError,
                `Request ${request_id} not found`,
              );
            }

            return {
              content: [
                {
                  type: 'text',
                  text: JSON.stringify(status, null, 2),
                },
              ],
            };
          }

          default:
            throw new McpError(
              ErrorCode.MethodNotFound,
              `Unknown tool: ${request.params.name}`,
            );
        }
      } catch (error) {
        if (axios.isAxiosError(error)) {
          throw new McpError(
            ErrorCode.InternalError,
            `Perplexity API error: ${
              error.response?.data?.error?.message || error.message
            }`,
          );
        }
        throw error;
      }
    });
  }

  async run() {
    const transport = new StdioServerTransport();
    console.log('Connecting to MCP transport...');
    try {
      await this.server.connect(transport);
      console.log('Perplexity MCP server successfully connected');
    } catch (error) {
      console.error('Failed to connect to MCP transport:', error);
      process.exit(1);
    }
  }
}

const config: ServerConfig = {
  apiKey: process.env.PERPLEXITY_API_KEY!,
  baseURL: 'https://api.perplexity.ai',
  rateLimit: {
    maxRequests: 50,
    windowSize: 60000, // 1 minute
  },
  timeout: 120000, // Increased to 2 minutes
  dbPath: join(homedir(), '.perplexity-mcp', 'chat_history.db'),
};

// Add global error handler
process.on('uncaughtException', (error) => {
  console.error('Uncaught Exception:', error);
  // Don't exit process, just log the error
});

process.on('unhandledRejection', (reason, promise) => {
  console.error('Unhandled Rejection at:', promise, 'reason:', reason);
  // Don't exit process, just log the error
});

const server = new PerplexityServer(config);
server.run().catch(console.error);
