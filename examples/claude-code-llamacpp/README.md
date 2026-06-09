# Claude Code + Olla + llama.cpp Integration Example

Use Claude Code with a local llama.cpp server through Olla's Anthropic API translation layer.

## Architecture

```
Claude Code  --[Anthropic API]-->  Olla :40114  --[OpenAI API]-->  llama.cpp :8080
             <--[Anthropic API]--             <--[OpenAI API]--
```

Olla receives Anthropic Messages API requests from Claude Code. llama.cpp has native Anthropic support (build b4847+), so eligible requests are forwarded directly (passthrough) with no translation overhead.

## Prerequisites

- Docker and Docker Compose
- Claude Code ([Installation Guide](https://code.claude.com/docs/en/overview))
- A GGUF model file (see Model Setup below)

## Model Setup

Download a GGUF model before starting:

```bash
# Create models directory
mkdir -p models

# Qwen3 8B - modern coding-capable baseline (~5GB Q4_K_M)
wget -P models https://huggingface.co/Qwen/Qwen3-8B-GGUF/resolve/main/Qwen3-8B-Q4_K_M.gguf

# Mistral Magistral Small - strong reasoning (~9GB Q5_K_M)
wget -P models https://huggingface.co/mistralai/Magistral-Small-2509-GGUF/resolve/main/Magistral-Small-2509-Q5_K_M.gguf
```

Update `compose.yaml` to reference your chosen model (the `--model` argument in the `llama-cpp` service).

## Quick Start

### 1. Download Model

See Model Setup above.

### 2. Update compose.yaml

Edit `compose.yaml` and set the model filename:

```yaml
command:
  - "--model"
  - "/models/Qwen3-8B-Q4_K_M.gguf"  # Update to your filename
```

### 3. Start Services

```bash
docker compose up -d
```

Wait 10-60 seconds for llama.cpp to load the model.

### 4. Verify Setup

```bash
./test.sh
```

### 5. Configure Claude Code

**Option A: Environment Variables** (simplest)

```bash
export ANTHROPIC_BASE_URL="http://localhost:40114/olla/anthropic"
export ANTHROPIC_AUTH_TOKEN="not-required"
```

Set `ANTHROPIC_AUTH_TOKEN` to any non-empty string; Olla does not enforce authentication.

**Option B: Project settings.json**

Create `.claude/settings.json` in your project directory:

```json
{
  "$schema": "https://json.schemastore.org/claude-code-settings.json",
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:40114/olla/anthropic",
    "ANTHROPIC_AUTH_TOKEN": "not-required",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "Qwen3-8B-Q4_K_M",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "Qwen3-8B-Q4_K_M"
  }
}
```

Replace the model name with whatever llama.cpp reports via `/v1/models`.

`settings.json` uses a real Claude Code schema. The `env` block sets environment variables for every session in that project. Claude Code does not read a generic `config.json`.

**Model override variables**

| Variable | Tier |
|---|---|
| `ANTHROPIC_DEFAULT_SONNET_MODEL` | Sonnet (default for most tasks) |
| `ANTHROPIC_DEFAULT_HAIKU_MODEL` | Haiku (fast/cheap tasks) |
| `ANTHROPIC_DEFAULT_OPUS_MODEL` | Opus (complex reasoning) |

### 6. Use Claude Code

```bash
claude
```

## Files

| File | Description |
|---|---|
| `compose.yaml` | Docker Compose for Olla + llama.cpp |
| `olla.yaml` | Olla configuration |
| `test.sh` | Test script |
| `README.md` | This file |

## Configuration

### llama.cpp Server Options

Edit `compose.yaml` to customise llama.cpp:

```yaml
services:
  llama-cpp:
    command:
      - "--model"
      - "/models/your-model.gguf"
      - "--ctx-size"
      - "8192"              # Context window size
      - "--n-gpu-layers"
      - "0"                 # CPU-only (set > 0 for GPU offload)
      - "--threads"
      - "8"                 # CPU threads
      - "--batch-size"
      - "512"
      - "--port"
      - "8080"
      - "--host"
      - "0.0.0.0"
```

**Key parameters**:

- `--ctx-size`: Context window (2048, 4096, 8192, ...)
- `--n-gpu-layers`: Layers to offload to GPU (0 = CPU only, -1 = all layers)
- `--threads`: CPU threads (match your physical core count)
- `--batch-size`: Batch size (higher = faster, more memory)

### GPU Support

For NVIDIA GPUs:

```yaml
services:
  llama-cpp:
    image: ghcr.io/ggml-org/llama.cpp:server-cuda
    command:
      - "--n-gpu-layers"
      - "-1"                # Offload all layers to GPU
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
```

For AMD GPU:
```yaml
services:
  llama-cpp:
    image: ghcr.io/ggml-org/llama.cpp:server-rocm
```

For Apple Metal:
```yaml
services:
  llama-cpp:
    image: ghcr.io/ggml-org/llama.cpp:server-metal
```

## Troubleshooting

### Model Not Loading

```bash
docker compose logs llama-cpp
```

Common causes: incorrect model path in `compose.yaml`, model file missing from `./models/`, insufficient memory.

```bash
# Verify the file is there
ls -lh models/
```

### Out of Memory

- Use a smaller model (3B instead of 7B)
- Use a lower quantisation level (Q4_K_M uses ~50% less memory than F16)
- Reduce context size:
  ```yaml
  - "--ctx-size"
  - "2048"
  ```

### Slow Performance

For CPU inference:
```yaml
- "--threads"
- "12"          # Use more threads
- "--batch-size"
- "1024"
```

For GPU offload (if available):
```yaml
- "--n-gpu-layers"
- "-1"          # All layers on GPU
```

### Connection Refused

Check llama.cpp is running and the model loaded successfully:

```bash
curl http://localhost:8080/health
docker compose logs llama-cpp
```

## Advanced: Multiple Models

Run multiple llama.cpp instances on different ports and add them all to `olla.yaml`:

```yaml
discovery:
  static:
    endpoints:
      - url: "http://llama-cpp-small:8080"
        name: "small-model"
        type: "llamacpp"
        priority: 100
        model_url: "/v1/models"
        health_check_url: "/health"
        check_interval: 2s
        check_timeout: 1s

      - url: "http://llama-cpp-large:8081"
        name: "large-model"
        type: "llamacpp"
        priority: 50
        model_url: "/v1/models"
        health_check_url: "/health"
        check_interval: 2s
        check_timeout: 1s
```

## Monitoring

```bash
# Service logs
docker compose logs -f olla
docker compose logs -f llama-cpp

# Olla status
curl http://localhost:40114/internal/status | jq

# llama.cpp metrics (Prometheus format)
curl http://localhost:8080/metrics
```

## Related Examples

- [Claude Code + Ollama](../claude-code-ollama/) - Easier setup; Ollama manages model downloads
