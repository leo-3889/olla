---
title: OpenCode + Olla Integration
description: Configure OpenCode AI coding assistant to use local LLM models via Olla's API endpoints. Supports both OpenAI and Anthropic API formats for load-balancing, failover, and model unification across Ollama, LM Studio, vLLM, and other backends.
keywords: OpenCode, Olla, SST, AI SDK, local LLM, Ollama, vLLM, LM Studio, load balancing, API translation, coding assistant
---

# OpenCode Integration with Olla

OpenCode is an open-source AI coding assistant that can connect to Olla's API endpoints, enabling you to use local LLM infrastructure with flexible OpenAI or Anthropic API compatibility.

**Minimal OpenCode config** (`~/.config/opencode/opencode.json`):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "olla": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Olla",
      "options": {
        "baseURL": "http://localhost:40114/olla/openai/v1"
      },
      "models": {
        "llama3.2:latest": { "name": "Llama 3.2" }
      }
    }
  }
}
```

The `models` map is required. OpenCode will not expose any models for the provider if this field is absent. Keys must match the model IDs that appear in `/v1/models` responses from your backend.

**What you get via Olla**

* One stable API base URL for all local backends
* Priority/least-connections load-balancing and health checks
* Streaming passthrough
* Unified `/v1/models` across providers
* Support for both OpenAI and Anthropic API formats

## Overview

<table>
    <tr>
        <th>Project</th>
        <td><a href="https://github.com/sst/opencode">OpenCode</a> (SST fork of archived AI coding assistant)</td>
    </tr>
    <tr>
        <th>Status</th>
        <td>Original project archived; actively maintained by <a href="https://sst.dev">SST</a></td>
    </tr>
    <tr>
        <th>Integration Type</th>
        <td>Frontend UI / Terminal Coding Assistant</td>
    </tr>
    <tr>
        <th>Connection Method</th>
        <td>AI SDK with OpenAI-compatible provider</td>
    </tr>
    <tr>
        <th>
          Features Supported <br/>
          <small>(via Olla)</small>
        </th>
        <td>
            <ul>
                <li>Chat Completions</li>
                <li>Code Generation &amp; Editing</li>
                <li>Streaming Responses</li>
                <li>Model Selection</li>
                <li>Tool Use (Function Calling)</li>
            </ul>
        </td>
    </tr>
    <tr>
        <th>Configuration</th>
        <td>
            Edit <code>~/.config/opencode/opencode.json</code> to add Olla provider <br/>
            <pre>baseURL: "http://localhost:40114/olla/openai/v1"</pre>
        </td>
    </tr>
    <tr>
        <th>Example</th>
        <td>
            Complete working example available in <code>examples/opencode-lmstudio/</code>
        </td>
    </tr>
</table>

## What is OpenCode?

OpenCode is an open-source AI coding assistant built with Go that provides:

- **Intelligent Code Generation**: Context-aware code suggestions and completions
- **Multi-file Editing**: Understands and modifies entire codebases
- **Terminal Integration**: Works directly in your development environment
- **Flexible API Support**: Compatible with OpenAI-compatible and other AI provider APIs

**Repository**: [https://github.com/sst/opencode](https://github.com/sst/opencode)

**Project Status**: The original OpenCode project was archived by the creator. It is now actively maintained as a fork by the SST (Serverless Stack) team. The SST fork continues to receive updates and improvements.

By default, OpenCode connects to cloud APIs (Anthropic, OpenAI, etc.). With Olla's API compatibility you can redirect it to local models whilst maintaining full functionality.

## Architecture

```text
┌──────────────┐    OpenAI-compatible   ┌──────────┐    OpenAI API    ┌─────────────────────┐
│  OpenCode    │    API requests        │   Olla   │─────────────────▶│ Ollama :11434       │
│  (Terminal)  │───────────────────────▶│  :40114  │  /v1/*           └─────────────────────┘
│              │  /olla/openai/v1/*     │          │                   ┌─────────────────────┐
│              │                        │  • Load  │─────────────────▶│ LM Studio :1234     │
│              │◀──────────────────────│    Balancing                 └─────────────────────┘
└──────────────┘    Streamed response  │  • Health │                   ┌─────────────────────┐
                                        │    Checks │─────────────────▶│ vLLM :8000          │
                                        └──────────┘                   └─────────────────────┘
                                             │
                                             ├─ Routes to healthy backend
                                             └─ Unified model registry
```

## Prerequisites

Before starting, ensure you have:

1. **OpenCode Installed**
   - SST fork: [https://github.com/sst/opencode](https://github.com/sst/opencode)
   - Install via curl (recommended): `curl -fsSL https://opencode.ai/install | bash`
   - Or via npm: `npm install -g opencode-ai`
   - Or via Homebrew: `brew install anomalyco/tap/opencode`

2. **Olla Running**
   - Installed and configured (see [Installation Guide](../../getting-started/installation.md))
   - Default port: 40114

3. **At Least One Backend**
   - Ollama, LM Studio, vLLM, llama.cpp, or any OpenAI-compatible endpoint
   - With at least one model loaded/available

4. **Docker & Docker Compose** (for the Docker quick start)
   - Required only if following the Docker-based setup below

## Quick Start (Docker Compose)

A complete working example lives in [`examples/opencode-lmstudio/`](https://github.com/thushan/olla/tree/main/examples/opencode-lmstudio/). The steps below walk through it manually.

### 1. Create Project Directory

```bash
mkdir opencode-olla
cd opencode-olla
```

### 2. Create Configuration Files

Create **`compose.yaml`**:

```yaml
services:
  olla:
    image: ghcr.io/thushan/olla:latest
    container_name: olla
    restart: unless-stopped
    ports:
      - "40114:40114"
    volumes:
      - ./olla.yaml:/app/config.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "--quiet", "--tries=1", "--spider", "http://localhost:40114/internal/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s

  ollama:
    image: ollama/ollama:latest
    container_name: ollama
    restart: unless-stopped
    volumes:
      - ollama_data:/root/.ollama

volumes:
  ollama_data:
    driver: local
```

> **Note**: This example uses Ollama because LM Studio does not publish an official Docker image. To use LM Studio instead, run it on the host and point the endpoint URL at `http://host.docker.internal:1234` (Windows/macOS) or the Docker bridge IP (Linux). See `examples/opencode-lmstudio/` for the LM Studio-specific setup.

Create **`olla.yaml`**:

```yaml
server:
  host: 0.0.0.0
  port: 40114

proxy:
  engine: olla                 # high-performance engine; sherpa is maintenance-mode
  load_balancer: least-connections
  response_timeout: 1800s      # 30 min for long generations
  read_timeout: 600s

# OpenAI is the native format, no translator needed.
# Anthropic translator enables the /olla/anthropic/v1/messages endpoint.
translators:
  anthropic:
    enabled: true

discovery:
  type: static
  static:
    endpoints:
      - url: http://ollama:11434
        name: ollama-local
        type: ollama
        priority: 100
        health_check_url: /api/tags
        check_interval: 30s
        check_timeout: 5s

logging:
  level: info
  format: json
```

Key points about the Olla config:

- `server.rate_limits` (under `server:`, not a top-level `security:` block) controls rate limiting.
- Endpoint health fields are flat: `health_check_url`, `check_interval`, `check_timeout`. There is no nested `health_check:` block.
- Only `translators.anthropic` exists as a translator. OpenAI is the native format, not a translator.
- `proxy.write_timeout` does not exist. Use `server.write_timeout: 0s` if you need to override it (0 disables the server-level write timeout, which is required for streaming).

### 3. Start Services

```bash
docker compose up -d
```

Wait for services to be healthy:

```bash
docker compose ps
```

### 4. Load a Model

```bash
docker exec ollama ollama pull llama3.2:latest

# Or a coding-focused model:
docker exec ollama ollama pull qwen2.5-coder:7b
```

### 5. Verify Olla Setup

```bash
# Health check
curl http://localhost:40114/internal/health

# List available models (OpenAI format)
curl http://localhost:40114/olla/openai/v1/models | jq

# Test a completion
curl -X POST http://localhost:40114/olla/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:latest",
    "messages": [{"role":"user","content":"Hello from Olla"}],
    "max_tokens": 100
  }' | jq
```

### 6. Configure OpenCode

OpenCode reads from `~/.config/opencode/opencode.json` (global config) and merges any `opencode.json` found in your project root. Create the global config:

```bash
mkdir -p ~/.config/opencode
```

**`~/.config/opencode/opencode.json`**:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "olla": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Olla",
      "options": {
        "baseURL": "http://localhost:40114/olla/openai/v1",
        "apiKey": "not-required"
      },
      "models": {
        "llama3.2:latest": { "name": "Llama 3.2" },
        "qwen2.5-coder:7b": { "name": "Qwen 2.5 Coder 7B" }
      }
    }
  }
}
```

**Important**: The `models` map is required. Without it, OpenCode won't know which models to show for the provider. The keys must match the model IDs returned by Olla's `/v1/models` endpoint. Check with:

```bash
curl http://localhost:40114/olla/openai/v1/models | jq '.data[].id'
```

### 7. Start OpenCode

```bash
opencode
```

Select a model using the `/models` command within the OpenCode UI. Try prompts like:

- "Write a Python function to calculate factorial"
- "Explain this code: [paste code]"
- "Help me refactor this function"

## Configuration Reference

### OpenCode Configuration File

**Location**: `~/.config/opencode/opencode.json` (global), or `opencode.json` in your project root (project-level, highest precedence).

**Configuration merging**: OpenCode merges configs from multiple sources, where later configs override only conflicting keys. This lets you have a global Olla provider and project-specific model selections.

**Complete Example**:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "olla/qwen2.5-coder:7b",
  "provider": {
    "olla": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Olla (local)",
      "options": {
        "baseURL": "http://localhost:40114/olla/openai/v1",
        "apiKey": "not-required"
      },
      "models": {
        "llama3.2:latest": {
          "name": "Llama 3.2"
        },
        "qwen2.5-coder:7b": {
          "name": "Qwen 2.5 Coder 7B",
          "limit": {
            "context": 32768,
            "output": 8192
          }
        },
        "qwen2.5-coder:32b": {
          "name": "Qwen 2.5 Coder 32B"
        }
      }
    }
  }
}
```

**Configuration Fields**:

| Field | Type | Description |
|-------|------|-------------|
| `provider.<id>.npm` | string | AI SDK package. Use `@ai-sdk/openai-compatible` for Olla. |
| `provider.<id>.name` | string | Display name shown in the OpenCode UI. |
| `provider.<id>.options.baseURL` | string | Olla OpenAI endpoint URL. |
| `provider.<id>.options.apiKey` | string | Any value; Olla does not validate keys by default. |
| `provider.<id>.models` | object | Map of model IDs to display config. **Required.** |
| `provider.<id>.models.<id>.name` | string | Model display name. |
| `provider.<id>.models.<id>.limit` | object | Optional `context`/`output` token limits. |
| `model` | string | Default model in `provider_id/model_id` format. |

**Note**: `temperature` and `maxTokens` are not valid top-level fields in `opencode.json`. Per-model options (reasoning effort, etc.) are configured inside the provider's `models.<id>.options` block. See the [OpenCode docs](https://opencode.ai/docs/config/) for full reference.

### Olla Configuration

Edit `olla.yaml` to customise:

**Load Balancing Strategy**:
```yaml
proxy:
  load_balancer: least-connections  # round-robin, least-connections, priority
```

- **priority**: Uses highest priority backend first (recommended for local + fallback)
- **round-robin**: Distributes evenly across all backends
- **least-connections**: Routes to backend with fewest active requests (default)

**Timeout Configuration**:
```yaml
proxy:
  response_timeout: 1800s  # Max time for full response (30 minutes)
  read_timeout: 600s       # Max time for reading response body
```

`proxy.write_timeout` does not exist. If you need to adjust the server-level write deadline (to avoid cuts in long-running streams), set `server.write_timeout: 0s` which disables it entirely.

**Streaming Optimisation**:
```yaml
proxy:
  profile: streaming  # Optimised buffer sizes and timeouts for streaming
```

**Rate Limiting** (under `server:`, not a top-level `security:` block):
```yaml
server:
  rate_limits:
    global_requests_per_minute: 1000
    per_ip_requests_per_minute: 100
    burst_size: 50
    health_requests_per_minute: 1000
    cleanup_interval: 5m
    trust_proxy_headers: false
```

**Multiple Backends**:
```yaml
discovery:
  static:
    endpoints:
      - url: http://lmstudio-host:1234
        name: lmstudio-gpu
        type: lmstudio
        priority: 100
        health_check_url: /v1/models
        check_interval: 30s
        check_timeout: 5s

      - url: http://ollama:11434
        name: local-ollama
        type: ollama
        priority: 90
        health_check_url: /api/tags
        check_interval: 30s
        check_timeout: 5s

      - url: http://vllm:8000
        name: vllm-cluster
        type: vllm
        priority: 80
        health_check_url: /health
        check_interval: 30s
        check_timeout: 5s
```

## Usage

### Selecting Models

Use the `/models` command within OpenCode to pick from the models you've declared in your `opencode.json`. To set a default at startup, add a top-level `model` field in your config:

```json
{
  "model": "olla/qwen2.5-coder:7b"
}
```

The format is `provider_id/model_id`, where `provider_id` is the key you used under `"provider"` in `opencode.json`.

### List Available Model IDs

```bash
# Check which model IDs Olla sees from your backends
curl http://localhost:40114/olla/openai/v1/models | jq '.data[].id'
```

Use the exact IDs returned here as keys in your `models` map.

### Basic Usage

```text
# In OpenCode terminal
> Write a Python function that calculates the Fibonacci sequence recursively
> Refactor the user authentication in auth.js to use async/await
> Explain this code: [paste code snippet]
> I'm getting this error: [paste error], help me fix it
```

## Docker Deployment (Production)

### Production olla.yaml

```yaml
server:
  host: 0.0.0.0
  port: 40114
  rate_limits:
    global_requests_per_minute: 1000
    per_ip_requests_per_minute: 100
    burst_size: 50
    health_requests_per_minute: 1000
    cleanup_interval: 5m

proxy:
  engine: olla
  load_balancer: least-connections
  response_timeout: 1800s
  read_timeout: 600s
  profile: streaming

translators:
  anthropic:
    enabled: true

discovery:
  type: static
  static:
    endpoints:
      - url: http://ollama:11434
        name: local-ollama
        type: ollama
        priority: 100
        health_check_url: /api/tags
        check_interval: 30s
        check_timeout: 5s

logging:
  level: info
  format: json
```

### Enhanced compose.yaml

```yaml
services:
  olla:
    image: ghcr.io/thushan/olla:latest
    container_name: olla
    restart: unless-stopped
    ports:
      - "40114:40114"
    volumes:
      - ./olla.yaml:/app/config.yaml:ro
    healthcheck:
      test: ["CMD", "wget", "--quiet", "--tries=1", "--spider", "http://localhost:40114/internal/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
    networks:
      - olla-network

  ollama:
    image: ollama/ollama:latest
    container_name: ollama
    restart: unless-stopped
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
    volumes:
      - ollama_data:/root/.ollama
    networks:
      - olla-network

volumes:
  ollama_data:
    driver: local

networks:
  olla-network:
    driver: bridge
```

## Model Selection Tips

### Recommended Models for OpenCode

**Code-Focused Models**:
- `qwen2.5-coder:32b` - Excellent for code generation and understanding
- `qwen2.5-coder:7b` - Good balance of speed and quality
- `deepseek-coder-v2:latest` - Strong multi-language support
- `codellama:34b` - Meta's specialised coding model
- `phi3.5:latest` - Efficient, good for quick tasks

**General Purpose (Code + Chat)**:
- `llama3.3:latest` - Well-balanced, fast
- `qwen3:32b` - Strong multi-task performance

**Performance vs Quality Trade-offs**:

| Model Size | Response Time | Quality | Memory Required |
|------------|---------------|---------|-----------------|
| 3-8B       | Fast (< 2s)   | Good    | 4-8 GB          |
| 13-20B     | Medium (2-5s) | Better  | 12-16 GB        |
| 30-70B     | Slow (5-15s)  | Best    | 24-64 GB        |

**Loading Models**:

```bash
# Ollama
docker exec ollama ollama pull qwen2.5-coder:7b

# Check loaded models
docker exec ollama ollama list
```

## Troubleshooting

### OpenCode Can't Connect to Olla

**Check configuration file**:
```bash
cat ~/.config/opencode/opencode.json
# Verify baseURL points to the correct Olla endpoint
```

**Test Olla directly**:
```bash
curl http://localhost:40114/internal/health
```

**Check endpoint health**:
```bash
curl http://localhost:40114/internal/status/endpoints | jq
```

### No Models Available

**Verify model IDs**: The keys in your `models` map must match exactly what Olla returns:
```bash
curl http://localhost:40114/olla/openai/v1/models | jq '.data[].id'
```

**Check backend health**:
```bash
curl http://localhost:40114/internal/status/endpoints | jq
```

**Verify backend directly**:
```bash
# Ollama
curl http://localhost:11434/api/tags

# LM Studio
curl http://localhost:1234/v1/models
```

### Slow Responses

**Ensure high-performance proxy engine is active**:
```yaml
proxy:
  engine: olla   # not sherpa; sherpa is maintenance-mode
  profile: streaming
```

**Increase timeout for large models**:
```yaml
proxy:
  response_timeout: 3600s  # 1 hour
  read_timeout: 1200s
```

**Check backend performance**:
```bash
docker stats ollama
docker exec ollama nvidia-smi
```

### Connection Refused

**From OpenCode to Olla**:
```bash
curl http://localhost:40114/internal/health
```

**From Olla to Backend (Docker)**:
```bash
docker exec olla wget -q -O- http://ollama:11434/api/tags
# If this fails, check the Docker network
docker network inspect opencode-olla_default
```

### Streaming Issues

**Enable streaming profile**:
```yaml
proxy:
  profile: streaming
```

**Test streaming directly**:
```bash
curl -N -X POST http://localhost:40114/olla/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:latest",
    "messages": [{"role":"user","content":"Count to 5"}],
    "stream": true
  }'
```

### API Key Issues

Olla doesn't enforce API keys by default. If OpenCode requires one, set any placeholder value:

```json
{
  "provider": {
    "olla": {
      "options": {
        "apiKey": "not-required"
      }
    }
  }
}
```

### Anthropic Endpoint Issues

If you want to use the Anthropic Messages API format (via `@ai-sdk/anthropic` pointing at Olla), be aware there is an [active bug in OpenCode](https://github.com/sst/opencode/issues/21737) where the API key is dropped at runtime when using a custom `baseURL` with the `@ai-sdk/anthropic` package. The workaround is to use `@ai-sdk/openai-compatible` with the OpenAI endpoint (`/olla/openai/v1`) instead. It works reliably and requires no translation overhead.

If you specifically need the Anthropic Messages format, point a separate provider at Olla's `/olla/anthropic/v1` endpoint. Olla serves that format for every backend (passthrough where native support exists, translation otherwise), so the backend does not need to speak Anthropic itself. See the [Anthropic API Translation](../api-translation/anthropic.md) docs for how passthrough and translation are selected.

## Advanced Configuration

### Using Non-Docker Backends

**olla.yaml with host services**:
```yaml
discovery:
  static:
    endpoints:
      # macOS/Windows: Use host.docker.internal
      - url: http://host.docker.internal:1234
        name: lmstudio-local
        type: lmstudio
        priority: 100
        health_check_url: /v1/models
        check_interval: 30s
        check_timeout: 5s

      # Linux: Use host IP directly
      - url: http://192.168.1.100:11434
        name: ollama-workstation
        type: ollama
        priority: 90
        health_check_url: /api/tags
        check_interval: 30s
        check_timeout: 5s
```

### Load Balancing Across Multiple GPUs

```yaml
discovery:
  static:
    endpoints:
      - url: http://gpu1-ollama:11434
        name: gpu1
        type: ollama
        priority: 100
        health_check_url: /api/tags
        check_interval: 30s
        check_timeout: 5s

      - url: http://gpu2-ollama:11434
        name: gpu2
        type: ollama
        priority: 100
        health_check_url: /api/tags
        check_interval: 30s
        check_timeout: 5s

      - url: http://gpu3-vllm:8000
        name: gpu3-vllm
        type: vllm
        priority: 90
        health_check_url: /health
        check_interval: 30s
        check_timeout: 5s

proxy:
  load_balancer: least-connections  # Distribute load evenly
```

### Monitoring and Observability

```bash
# Endpoint status
curl http://localhost:40114/internal/status/endpoints | jq

# Model statistics
curl http://localhost:40114/internal/status/models | jq

# Health
curl http://localhost:40114/internal/health
```

**View logs**:
```bash
docker compose logs -f olla
docker compose logs olla | grep -i error
```

**Custom logging** (under `logging:`, not under `server:`):
```yaml
logging:
  level: debug   # debug, info, warn, error
  format: json   # json, text
  output: stdout # stdout, file
```

## Best Practices

### 1. Model Management

- **Start small**: Test with smaller models (3-8B) before using larger ones
- **Specialised models**: Use code-specific models (e.g., `qwen2.5-coder`) for better results
- **Clean up**: Remove unused models to save disk space
- **Keep models map current**: Update your `opencode.json` models map whenever you pull new models

### 2. Performance Optimisation

- **GPU acceleration**: Use CUDA-enabled backend images for GPU support
- **Resource limits**: Set Docker memory/CPU limits to prevent host resource exhaustion
- **Olla engine**: Use `engine: olla` (default) for better connection handling
- **Streaming profile**: Enable `profile: streaming` for real-time response feel

### 3. Development Workflow

- **Local-first**: Configure highest priority for local backends
- **Fallback remotes**: Add lower-priority remote endpoints for reliability
- **Model isolation**: Separate models for different tasks (code vs chat vs analysis)
- **Version control**: Keep `olla.yaml` in your project repo; keep OpenCode config in your home dir

### 4. Security

- **Network isolation**: Use Docker networks to isolate services
- **Rate limiting**: Enable `server.rate_limits` in production to prevent abuse
- **No public exposure**: Don't expose Olla directly to the internet without authentication
- **API gateway**: Use nginx/Traefik with auth for external access

### 5. Cost Efficiency

- **Local models**: Save on API costs whilst maintaining privacy
- **Model caching**: Keep frequently used models loaded
- **Resource sharing**: One Olla instance can serve multiple developers

## OpenCode vs Claude Code vs Crush CLI

| Feature | OpenCode | Claude Code | Crush CLI |
|---------|----------|-------------|-----------|
| **License** | Open Source (archived original) | Proprietary | Open Source |
| **Maintenance** | SST fork active | Anthropic official | Charmbracelet active |
| **API Support** | OpenAI-compatible (primary) | Anthropic only | Both |
| **Platform** | Go | Unknown | Go |
| **Configuration** | JSON config file | Environment variables | JSON config file |
| **Best For** | Customisable workflows | Official Anthropic support | Modern terminal UI |

## Next Steps

### Related Documentation

- **[Anthropic Messages API Reference](../../api-reference/anthropic.md)** - Complete API documentation
- **[OpenAI API Reference](../../api-reference/openai.md)** - OpenAI endpoint documentation
- **[API Translation Concept](../../concepts/api-translation.md)** - How translation works
- **[Load Balancing](../../concepts/load-balancing.md)** - Understanding request distribution
- **[Model Routing](../../concepts/model-routing.md)** - How models are selected

### Integration Examples

- **[OpenCode + LM Studio Example](https://github.com/thushan/olla/tree/main/examples/opencode-lmstudio/)** - Complete setup
- **[Claude Code + Ollama Example](https://github.com/thushan/olla/tree/main/examples/claude-code-ollama/)** - Similar setup pattern
- **[Claude Code Integration](claude-code.md)** - Official Anthropic CLI
- **[Crush CLI Integration](crush-cli.md)** - Modern terminal assistant

### Backend Guides

- **[Ollama Integration](../backend/ollama.md)** - Ollama-specific configuration
- **[LM Studio Integration](../backend/lmstudio.md)** - LM Studio setup
- **[vLLM Integration](../backend/vllm.md)** - High-performance inference

### Advanced Topics

- **[Health Checking](../../concepts/health-checking.md)** - Endpoint monitoring
- **[Circuit Breaking](../../development/circuit-breaker.md)** - Failure handling
- **[Provider Metrics](../../concepts/provider-metrics.md)** - Performance metrics

---

## Support

**Community**:
- GitHub Issues: [https://github.com/thushan/olla/issues](https://github.com/thushan/olla/issues)
- Discussions: [https://github.com/thushan/olla/discussions](https://github.com/thushan/olla/discussions)

**OpenCode Resources**:
- SST OpenCode: [https://github.com/sst/opencode](https://github.com/sst/opencode)
- OpenCode Docs: [https://opencode.ai/docs](https://opencode.ai/docs)
- SST: [https://sst.dev](https://sst.dev)

**Common Resources**:
- [Olla Project Home](../../index.md)
- [OpenAI API Reference](https://platform.openai.com/docs/api-reference)
- [Anthropic API Reference](https://docs.anthropic.com/en/api)

**Quick Help**:
```bash
# Verify setup
curl http://localhost:40114/internal/health
curl http://localhost:40114/olla/openai/v1/models | jq '.data[].id'

# Test message
curl -X POST http://localhost:40114/olla/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"llama3.2:latest","messages":[{"role":"user","content":"Hi"}]}' | jq

# Check logs
docker compose logs -f olla
```
