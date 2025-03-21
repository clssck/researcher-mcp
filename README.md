# MCP-researcher Server

A powerful research assistant that integrates with Cline and Claude Desktop! Leverages Perplexity AI for intelligent search, documentation retrieval, API discovery, and code modernization assistance - all while you code.

## Features

- **Seamless Context Tracking**: Maintains conversation history in SQLite database to provide coherent responses across multiple queries
- **Advanced Query Processing**: Uses Perplexity's Sonar models for sophisticated reasoning and detailed answers to complex questions
- **Intelligent Rate Management**: Implements adaptive rate limiting with exponential backoff to maximize API usage without hitting limits
- **High Performance Networking**: Optimizes API calls with connection pooling and automatic retry logic for reliable operation

## Tools

### 1. Search

Performs general search queries to get comprehensive information on any topic. The example shows how to use different detail levels (brief, normal, detailed) to get tailored responses.

### 2. Get Documentation

Retrieves documentation and usage examples for specific technologies, libraries, or APIs. The example demonstrates getting comprehensive documentation for React hooks, including best practices and common pitfalls.

### 3. Find APIs

Discovers and evaluates APIs that could be integrated into a project. The example shows finding payment processing APIs with detailed analysis of features, pricing, and integration complexity.

### 4. Check Deprecated Code

Analyzes code for deprecated features or patterns, providing migration guidance. The example demonstrates checking React class components and lifecycle methods for modern alternatives.

## Installation

### paste this part into claude directly if you want to, the ai can install it for you

1. First install Node.js if not already installed (from nodejs.org)

2. Clone the repo

3. Install dependencies and build

4. Get a Perplexity API key from [https://www.perplexity.ai/settings/api](https://www.perplexity.ai/settings/api)

5. Create the MCP settings file in the appropriate location for your OS:

6. To use with Claude Desktop, add the server config:

7. To use with Cline, add into mcpServers:

```json
{
  "mcpServers": {
    "perplexity-server": {
      "command": "node",
      "args": ["[path/to/researcher-mcp/build/index.js]"],
      "env": {
        "PERPLEXITY_API_KEY": "pplx-...",
        "PERPLEXITY_MODEL": "sonar-reasoning" // you can use different models
      },
      "disabled": false,
      "alwaysAllow": [],
      "autoApprove": [
        "search",
        "get_documentation",
        "find_apis",
        "check_deprecated_code",
        "get_request_status"
      ]
    }
  }
}
```

8. Build the server:
   npm run build
