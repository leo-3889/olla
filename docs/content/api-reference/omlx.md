# oMLX API

Proxy endpoints for oMLX inference servers running on Apple Silicon. Available through the `/olla/omlx/` prefix.

oMLX is a multi-model server: a single instance hosts many models concurrently, loading them on demand and evicting the least-recently-used ones under memory pressure. It is OpenAI-compatible on the wire and additionally implements the Anthropic Messages API, a reranking endpoint, and an oMLX-specific model-status endpoint.

## Endpoints Overview

| Method | URI | Description |
|--------|-----|-------------|
| GET | `/olla/omlx/health` | Health check |
| GET | `/olla/omlx/v1/models` | List available models |
| GET | `/olla/omlx/v1/models/status` | Loaded-model residency state |
| POST | `/olla/omlx/v1/chat/completions` | Chat completion |
| POST | `/olla/omlx/v1/completions` | Text completion |
| POST | `/olla/omlx/v1/embeddings` | Generate embeddings |
| POST | `/olla/omlx/v1/rerank` | Rerank documents |
| POST | `/olla/omlx/v1/responses` | OpenAI Responses API |

Anthropic-format requests are served through Olla's Anthropic endpoint (`/olla/anthropic/v1/messages`) in passthrough mode. See the [Anthropic API Reference](anthropic.md).

---

## GET /olla/omlx/health

Check oMLX server health status.

### Request

```bash
curl -X GET http://localhost:40114/olla/omlx/health
```

### Response

```json
{
  "status": "healthy"
}
```

---

## GET /olla/omlx/v1/models

List the models the oMLX server has discovered. The `id` is the configured alias where one is set, otherwise the model's directory name. `max_model_len` reports the effective context window and is preserved by Olla during discovery.

### Request

```bash
curl -X GET http://localhost:40114/olla/omlx/v1/models
```

### Response

```json
{
  "object": "list",
  "data": [
    {
      "id": "Qwen2.5-7B-Instruct-4bit",
      "object": "model",
      "created": 1705334400,
      "owned_by": "omlx",
      "max_model_len": 32768
    }
  ]
}
```

---

## GET /olla/omlx/v1/models/status

Report which models are currently resident in memory. This oMLX-specific endpoint is keyed by **directory name** (not alias) and is useful for understanding cold-start behaviour. Olla forwards it unchanged.

### Request

```bash
curl -X GET http://localhost:40114/olla/omlx/v1/models/status
```

### Response

```json
{
  "model_count": 3,
  "loaded_count": 1,
  "models": [
    {
      "id": "Qwen2.5-7B-Instruct-4bit",
      "loaded": true,
      "is_loading": false,
      "pinned": true,
      "estimated_size": 4500000000,
      "last_access": 1705334400.0
    },
    {
      "id": "Llama-3.2-3B-Instruct-4bit",
      "loaded": false,
      "is_loading": false,
      "pinned": false,
      "estimated_size": 1800000000,
      "last_access": null
    }
  ]
}
```

---

## POST /olla/omlx/v1/chat/completions

OpenAI-compatible chat completion. The first request for a model that is not resident triggers a load and may take several seconds.

### Request

```bash
curl -X POST http://localhost:40114/olla/omlx/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen2.5-7B-Instruct-4bit",
    "messages": [
      {
        "role": "system",
        "content": "You are a helpful AI assistant."
      },
      {
        "role": "user",
        "content": "What is MLX?"
      }
    ],
    "temperature": 0.7,
    "max_tokens": 300,
    "stream": false
  }'
```

### Response

```json
{
  "id": "chatcmpl-abc123",
  "object": "chat.completion",
  "created": 1705334400,
  "model": "Qwen2.5-7B-Instruct-4bit",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "MLX is an array framework for machine learning on Apple Silicon, built by Apple's machine learning research team. It uses the unified memory architecture of M-series chips for efficient GPU-accelerated computation."
      },
      "logprobs": null,
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 25,
    "completion_tokens": 40,
    "total_tokens": 65
  }
}
```

### Streaming Response

When `"stream": true`:

```text
data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1705334400,"model":"Qwen2.5-7B-Instruct-4bit","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1705334400,"model":"Qwen2.5-7B-Instruct-4bit","choices":[{"index":0,"delta":{"content":"MLX"},"finish_reason":null}]}

...

data: {"id":"chatcmpl-abc123","object":"chat.completion.chunk","created":1705334401,"model":"Qwen2.5-7B-Instruct-4bit","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]
```

---

## POST /olla/omlx/v1/completions

Text completion.

### Request

```bash
curl -X POST http://localhost:40114/olla/omlx/v1/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "Qwen2.5-7B-Instruct-4bit",
    "prompt": "Apple Silicon is designed for",
    "max_tokens": 200,
    "temperature": 0.8,
    "top_p": 0.95,
    "stream": false
  }'
```

### Response

```json
{
  "id": "cmpl-xyz789",
  "object": "text_completion",
  "created": 1705334400,
  "model": "Qwen2.5-7B-Instruct-4bit",
  "choices": [
    {
      "text": " high-performance, energy-efficient computing. The unified memory architecture lets the CPU, GPU, and Neural Engine share one memory pool, removing the overhead of copying data between processors.",
      "index": 0,
      "logprobs": null,
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 6,
    "completion_tokens": 36,
    "total_tokens": 42
  }
}
```

---

## POST /olla/omlx/v1/embeddings

Generate embeddings from an embedding model loaded by oMLX.

### Request

```bash
curl -X POST http://localhost:40114/olla/omlx/v1/embeddings \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mlx-community/bge-small-en-v1.5",
    "input": "MLX is optimised for Apple Silicon",
    "encoding_format": "float"
  }'
```

### Response

```json
{
  "object": "list",
  "data": [
    {
      "object": "embedding",
      "index": 0,
      "embedding": [0.0234, -0.0567, 0.0891, ...]
    }
  ],
  "model": "mlx-community/bge-small-en-v1.5",
  "usage": {
    "prompt_tokens": 8,
    "total_tokens": 8
  }
}
```

---

## POST /olla/omlx/v1/rerank

Rerank a set of documents against a query (Cohere/Jina-compatible), using a reranker model loaded by oMLX.

### Request

```bash
curl -X POST http://localhost:40114/olla/omlx/v1/rerank \
  -H "Content-Type: application/json" \
  -d '{
    "model": "mlx-community/bge-reranker-base",
    "query": "What is unified memory?",
    "documents": [
      "Apple Silicon shares memory between CPU and GPU.",
      "MLX is an array framework for Apple Silicon.",
      "Discrete GPUs use separate VRAM."
    ],
    "top_n": 2
  }'
```

### Response

```json
{
  "results": [
    {"index": 0, "relevance_score": 0.91},
    {"index": 1, "relevance_score": 0.44}
  ],
  "model": "mlx-community/bge-reranker-base",
  "usage": {
    "total_tokens": 38
  }
}
```

## Sampling Parameters

Standard OpenAI-compatible sampling parameters are supported.

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `temperature` | float | 1.0 | Sampling temperature |
| `top_p` | float | 1.0 | Nucleus sampling threshold |
| `top_k` | integer | - | Top-k sampling |
| `max_tokens` | integer | - | Maximum tokens to generate |
| `stop` | string/array | - | Stop sequences |
| `stream` | boolean | false | Enable streaming response |
| `frequency_penalty` | float | 0.0 | Frequency penalty |
| `presence_penalty` | float | 0.0 | Presence penalty |

## Configuration Example

```yaml
endpoints:
  - url: "http://192.168.0.100:8000"
    name: "omlx-server"
    type: "omlx"
    priority: 75
    model_url: "/v1/models"
    health_check_url: "/health"
    check_interval: 5s
    check_timeout: 2s
```

## Request Headers

All requests are forwarded with:

- `X-Olla-Request-ID` - Unique request identifier
- `X-Forwarded-For` - Client IP address
- Custom headers from endpoint configuration

## Response Headers

All responses include:

- `X-Olla-Endpoint` - Backend endpoint name (e.g., "omlx-server")
- `X-Olla-Model` - Model used for the request
- `X-Olla-Backend-Type` - Always "omlx" for these endpoints
- `X-Olla-Response-Time` - Total processing time
