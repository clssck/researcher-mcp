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
import axios, { AxiosError, AxiosInstance, AxiosResponse } from 'axios';
import axiosRetry, { isNetworkError } from 'axios-retry';
import CircuitBreaker from 'opossum';

// --- Constants for Prompts ---
const GET_DOCUMENTATION_PROMPT = (
  query: string,
  context?: string,
) => `Provide comprehensive documentation and usage examples for ${query}. ${
  context ? `Focus on: ${context}` : ''
} Include:
1. Basic overview and purpose
2. Key features and capabilities
3. Installation/setup if applicable
4. Common usage examples
5. Best practices
6. Common pitfalls to avoid
7. Links to official documentation if available`;

const FIND_APIS_PROMPT = (
  requirement: string,
  context?: string,
) => `Find and evaluate APIs that could be used for: ${requirement}. ${
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

const CHECK_DEPRECATED_CODE_PROMPT = (
  code: string,
  technology?: string,
) => `Analyze this code for deprecated features or patterns${
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

const SEARCH_PROMPT = (
  query: string,
  detail_level: 'brief' | 'normal' | 'detailed' = 'normal',
): string => {
  switch (detail_level) {
    case 'brief':
      return `Provide a brief, concise answer to: ${query}`;
    case 'detailed':
      return `Provide a comprehensive, detailed analysis of: ${query}. Include relevant examples, context, and supporting information where applicable.`;
    default:
      return `Provide a clear, balanced answer to: ${query}. Include key points and relevant context.`;
  }
};
// --- End Constants ---

// API Key check remains specific to Perplexity
const PERPLEXITY_API_KEY = process.env.PERPLEXITY_API_KEY;
if (!PERPLEXITY_API_KEY) {
  throw new Error('PERPLEXITY_API_KEY environment variable is required');
}

/**
 * Configuration settings for the PerplexityServer.
 */
interface ServerConfig {
  /** Perplexity API Key */
  apiKey: string;
  /** Base URL for the Perplexity API */
  baseURL: string;
  /** Rate limiting configuration for outgoing requests */
  rateLimit: {
    /** Maximum requests allowed within the window */
    maxRequests: number;
    /** Time window size in milliseconds */
    windowSize: number;
  };
  /** General timeout for API requests in milliseconds */
  timeout: number;
  /** Default Perplexity model to use (e.g., 'sonar-reasoning') */
  model?: string;
  /** Circuit breaker configuration options */
  circuitBreaker?: {
    /** Timeout for a single request in the breaker (ms) */
    timeout?: number;
    /** Percentage of errors to trip the circuit */
    errorThresholdPercentage?: number;
    /** Time window to wait before retrying after tripping (ms) */
    resetTimeout?: number;
  };
}

/**
 * Implements an MCP server that acts as a proxy to the Perplexity API,
 * providing tools for search, documentation lookup, API finding, and code checking.
 * Includes features like rate limiting, retries, connection pooling, and circuit breaking.
 */
class PerplexityServer {
  private server!: Server;
  private axiosInstance!: AxiosInstance;
  private config: ServerConfig;
  private breaker!: CircuitBreaker<
    [url: string, data: any, config?: any],
    AxiosResponse<any>
  >;

  /**
   * Creates an instance of PerplexityServer.
   * @param config The server configuration.
   */
  constructor(config: ServerConfig) {
    this.config = config;

    this.server = new Server(
      {
        name: 'research-mcp',
        version: '1.0.0',
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

    // --- Circuit Breaker Setup ---
    const breakerOptions: CircuitBreaker.Options = {
      timeout: this.config.circuitBreaker?.timeout ?? 115000, // Default 115s
      errorThresholdPercentage:
        this.config.circuitBreaker?.errorThresholdPercentage ?? 50, // Default 50%
      resetTimeout: this.config.circuitBreaker?.resetTimeout ?? 30000, // Default 30s
    };

    /**
     * The core function wrapped by the circuit breaker. Makes the actual POST request.
     * @param url The API endpoint URL.
     * @param data The request payload.
     * @param config Optional Axios request configuration (e.g., for streaming).
     * @returns A promise resolving to the Axios response.
     */
    const protectedApiCall = async (url: string, data: any, config?: any) => {
      // Note: We need to pass the stream handling config here if needed
      return this.axiosInstance.post(url, data, config);
    };

    this.breaker = new CircuitBreaker(protectedApiCall, breakerOptions);

    // Optional: Add event listeners for logging/monitoring
    this.breaker.on('open', () =>
      console.warn(`Circuit Breaker opened for Upstream API (Perplexity).`),
    );
    this.breaker.on('halfOpen', () =>
      console.log(`Circuit Breaker half-opened for Upstream API (Perplexity).`),
    );
    this.breaker.on('close', () =>
      console.log(`Circuit Breaker closed for Upstream API (Perplexity).`),
    );
    this.breaker.on('fallback', (result, error) =>
      console.error(
        `Circuit Breaker fallback executed for Upstream API due to: ${error.message}`,
      ),
    );

    // Fallback function if the circuit is open
    this.breaker.fallback((url, data, config, error) => {
      throw new McpError(
        ErrorCode.InternalError, // Fallback to InternalError as no specific upstream code exists
        `Circuit open: Upstream API (Perplexity) is unavailable or failing: ${
          error?.message || 'Circuit Open'
        }`,
      );
    });
    // --- End Circuit Breaker Setup ---

    this.setupToolHandlers();

    // Error handling
    this.server.onerror = (error) => console.error('[MCP Error]', error);
    process.on('SIGINT', async () => {
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

  private setupToolHandlers() {
    this.server.setRequestHandler(ListToolsRequestSchema, async () => ({
      tools: [
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
          case 'get_documentation': {
            const { query, context = '' } = request.params.arguments as {
              query: string;
              context?: string;
            };
            const prompt = GET_DOCUMENTATION_PROMPT(query, context);
            const responseText = await this._streamedApiCall(prompt);

            return {
              content: [
                {
                  type: 'text',
                  text: responseText,
                },
              ],
            };
          }

          case 'find_apis': {
            const { requirement, context = '' } = request.params.arguments as {
              requirement: string;
              context?: string;
            };
            const prompt = FIND_APIS_PROMPT(requirement, context);
            const responseText = await this._streamedApiCall(prompt);

            return {
              content: [
                {
                  type: 'text',
                  text: responseText,
                },
              ],
            };
          }

          case 'check_deprecated_code': {
            const { code, technology = '' } = request.params.arguments as {
              code: string;
              technology?: string;
            };
            const prompt = CHECK_DEPRECATED_CODE_PROMPT(code, technology);
            const responseText = await this._streamedApiCall(prompt);

            return {
              content: [
                {
                  type: 'text',
                  text: responseText,
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

            const prompt = SEARCH_PROMPT(query, detail_level);
            const responseText = await this._streamedApiCall(prompt);

            return {
              content: [
                {
                  type: 'text',
                  text: responseText,
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
      console.log('Researcher MCP server successfully connected');
    } catch (error) {
      console.error('Failed to connect to MCP transport:', error);
      process.exit(1);
    }
  }

  // --- Helper function for streamed API calls ---
  /**
   * Performs a streamed API call to Perplexity's chat completions endpoint
   * using the configured model and circuit breaker.
   * Accumulates the streamed response content.
   *
   * @param prompt The user prompt to send to the API.
   * @returns A promise resolving to the full text content from the stream.
   * @throws {McpError} If the circuit breaker fails or the stream encounters an error.
   */
  private async _streamedApiCall(prompt: string): Promise<string> {
    let responseStream;
    try {
      responseStream = await this.breaker.fire(
        '/chat/completions', // url
        {
          // data
          model: this.config.model || 'sonar-reasoning',
          messages: [{ role: 'user', content: prompt }],
          stream: true,
        },
        { responseType: 'stream' }, // config
      );
    } catch (error: any) {
      // Catch errors from the breaker itself (e.g., circuit open, timeout)
      if (error instanceof McpError) throw error; // Already handled by fallback
      throw new McpError(
        ErrorCode.InternalError,
        `Upstream API (Perplexity) stream error: ${error.message}`,
        error, // Pass original error if needed
      );
    }

    let fullContent = '';
    await new Promise<void>((resolve, reject) => {
      responseStream.data.on('data', (chunk: Buffer) => {
        try {
          const lines = chunk.toString('utf-8').split('\n');
          for (const line of lines) {
            if (line.startsWith('data: ')) {
              const dataString = line.substring(6).trim();
              if (dataString === '[DONE]') continue;
              const jsonData = JSON.parse(dataString);
              if (jsonData.choices && jsonData.choices[0].delta?.content) {
                fullContent += jsonData.choices[0].delta.content;
              }
            }
          }
        } catch (err) {
          console.error('Error processing stream chunk:', err);
        }
      });
      responseStream.data.on('end', resolve);
      responseStream.data.on('error', (err: Error) => {
        console.error('Stream error:', err);
        reject(
          new McpError(
            ErrorCode.InternalError,
            `Upstream API (Perplexity) stream error: ${err.message}`,
          ),
        );
      });
    });
    return fullContent;
  }
  // --- End Helper ---
}

const config: ServerConfig = {
  apiKey: process.env.PERPLEXITY_API_KEY!,
  baseURL: 'https://api.perplexity.ai',
  rateLimit: {
    maxRequests: 50,
    windowSize: 60000, // 1 minute
  },
  timeout: 120000, // Increased to 2 minutes
  model: process.env.PERPLEXITY_MODEL || 'sonar-reasoning',
  circuitBreaker: {
    timeout: process.env.CB_TIMEOUT
      ? parseInt(process.env.CB_TIMEOUT, 10)
      : undefined,
    errorThresholdPercentage: process.env.CB_ERROR_THRESHOLD
      ? parseInt(process.env.CB_ERROR_THRESHOLD, 10)
      : undefined,
    resetTimeout: process.env.CB_RESET_TIMEOUT
      ? parseInt(process.env.CB_RESET_TIMEOUT, 10)
      : undefined,
  },
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
