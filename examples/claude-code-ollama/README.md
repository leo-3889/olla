# Claude Code + Olla + Ollama Integration Example

Use Claude Code with a local Ollama instance through Olla's Anthropic API translation layer.

## Architecture

```
Claude Code  --[Anthropic API]-->  Olla :40114  --[Ollama API]-->  Ollama :11434
             <--[Anthropic API]--             <--[Ollama API]--
```

Olla receives Anthropic Messages API requests from Claude Code. Because Ollama has native Anthropic support (v0.14.0+), eligible requests are forwarded directly (passthrough) with no translation overhead.

## What This Example Includes

- Docker Compose setup with Olla and Ollama
- Pre-configured Olla with Anthropic translation enabled
- Example `.claude/settings.json` for Claude Code (`claude-code-config.example.json`)
- Test script to verify the setup

## Prerequisites

- Docker and Docker Compose
- Claude Code ([Installation Guide](https://code.claude.com/docs/en/overview))

## Quick Start

### 1. Start Services

```bash
docker compose up -d
```

Wait about 30 seconds for the services to start.

### 2. Pull a Model

```bash
# Recommended for coding tasks
docker exec ollama ollama pull llama3.2:latest

# Larger coding model (needs more RAM)
docker exec ollama ollama pull qwen2.5-coder:32b
```

### 3. Verify Setup

```bash
./test.sh
```

Expected output:

- Olla health check passing
- Models listed
- Non-streaming message working
- Streaming message working

### 4. Configure Claude Code

**Option A: Environment Variables** (simplest)

```bash
export ANTHROPIC_BASE_URL="http://localhost:40114/olla/anthropic"
export ANTHROPIC_AUTH_TOKEN="not-required"
```

Set `ANTHROPIC_AUTH_TOKEN` to any non-empty string; Olla does not enforce authentication.

**Option B: Project settings.json**

Copy the example settings file into a `.claude/` directory in your project:

```bash
mkdir -p .claude
cp claude-code-config.example.json .claude/settings.json
```

Edit `.claude/settings.json` and replace `llama3.2:latest` with whichever model you pulled.

`claude-code-config.example.json` is a valid `.claude/settings.json` file (schema: `https://json.schemastore.org/claude-code-settings.json`). The `env` block sets environment variables for every Claude Code session in that project. Claude Code does not read a generic `config.json`; the correct mechanism is this `settings.json` with an `env` block.

**Model override variables**

Claude Code maps internal tier aliases to model IDs via environment variables:

| Variable | Tier |
|---|---|
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | Sonnet (default for most tasks) |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | Haiku (fast/cheap tasks) |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | Opus (complex reasoning) |

Set these to whatever model you have loaded in Ollama.

### 5. Use Claude Code

```bash
claude
```

## Files

| File | Description |
|---|---|
| `compose.yaml` | Docker Compose configuration for Olla + Ollama |
| `olla.yaml` | Olla configuration with Anthropic translation enabled |
| `test.sh` | Test script to verify the setup |
| `claude-code-config.example.json` | Example `.claude/settings.json` for Claude Code |

## Verification

### Health Check

```bash
curl http://localhost:40114/internal/health
```

### Available Models

```bash
curl http://localhost:40114/olla/anthropic/v1/models | jq
```

### Test Chat Completion

```bash
curl -X POST http://localhost:40114/olla/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:latest",
    "max_tokens": 100,
    "messages": [
      {"role": "user", "content": "Say hello in one sentence"}
    ]
  }' | jq
```

### Streaming

```bash
curl -N -X POST http://localhost:40114/olla/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llama3.2:latest",
    "max_tokens": 50,
    "messages": [
      {"role": "user", "content": "Count from 1 to 3"}
    ],
    "stream": true
  }'
```

## Customisation

### Using Different Models

Pull additional models and update the model name in requests or the settings.json overrides:

```bash
docker exec ollama ollama pull mistral-nemo:latest
docker exec ollama ollama pull codellama:latest
```

### Adding Multiple Ollama Instances

Edit `olla.yaml`:

```yaml
discovery:
  static:
    endpoints:
      - url: "http://ollama:11434"
        name: "local-ollama"
        type: "ollama"
        priority: 100

      - url: "http://192.168.1.100:11434"  # Another machine
        name: "remote-ollama"
        type: "ollama"
        priority: 50                        # Lower priority (fallback)
```

### GPU Support (NVIDIA)

Uncomment the GPU section in `compose.yaml`:

```yaml
services:
  ollama:
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
```

## Troubleshooting

### Services Won't Start

```bash
docker compose logs olla
docker compose logs ollama
```

Common causes: port 40114 or 11434 already in use, Docker not running, insufficient disk space.

### No Models Available

```bash
docker exec ollama ollama list
docker exec ollama ollama pull llama3.2:latest
```

### Claude Code Can't Connect

```bash
# Verify the variable is set
echo $ANTHROPIC_BASE_URL
# Expected: http://localhost:40114/olla/anthropic

# Test Olla directly
curl http://localhost:40114/internal/health
```

### Connection Refused from Olla to Ollama

```bash
docker exec olla wget -q -O- http://ollama:11434/api/tags
```

### Slow Responses

- Use a smaller model: `docker exec ollama ollama pull llama3.2:3b`
- Enable GPU (see GPU Support above)
- Adjust Ollama parallelism inside the container:
  ```bash
  export OLLAMA_NUM_PARALLEL=2
  export OLLAMA_MAX_LOADED_MODELS=1
  ```

## Monitoring

```bash
# Olla logs
docker compose logs -f olla

# Ollama logs
docker compose logs -f ollama

# Olla status
curl http://localhost:40114/internal/status | jq

# Endpoint status
curl http://localhost:40114/internal/status/endpoints | jq
```

## Related Examples

- [Claude Code + llama.cpp](../claude-code-llamacpp/) - GGUF model inference
