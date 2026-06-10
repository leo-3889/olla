# Crush CLI + vLLM Integration Example

This example demonstrates how to set up [Crush CLI](https://github.com/charmbracelet/crush) with Olla as a proxy and load balancer for vLLM high-performance inference. Crush is configured as a custom provider that points at Olla, and the example shows both the OpenAI-compatible and Anthropic provider types so you can route the same vLLM backend through either API format.

## Architecture

```
┌─────────────┐    ┌──────────┐    ┌─────────────────┐
│  Crush CLI  │───▶│   Olla   │───▶│  vLLM Instance  │
│             │    │(Port     │    │  (Port 8000)    │
│ openai-compat│   │ 40114)   │    │                 │
│ or anthropic │   │          │    │ GPU-optimised   │
│ provider     │   │          │    │ PagedAttention  │
└─────────────┘    └──────────┘    └─────────────────┘
```

## Prerequisites

### GPU Requirements
vLLM requires NVIDIA GPU with compute capability 7.0 or higher:
- **Minimum**: RTX 2060 (6GB VRAM) for small models (3B-7B parameters)
- **Recommended**: RTX 3090/4090 (24GB VRAM) for medium models (13B-30B parameters)
- **Production**: A100/H100 (40GB+ VRAM) for large models (70B+ parameters)

### Software Requirements
- Docker with GPU support (NVIDIA Container Toolkit)
- Docker Compose v2.0+
- NVIDIA drivers 525.60.13 or newer
- Crush CLI installed ([Installation Guide](https://github.com/charmbracelet/crush))

## Quick Start

### 1. Verify GPU Support

```bash
# Check NVIDIA Docker runtime
docker run --rm --gpus all nvidia/cuda:12.1.0-base-ubuntu22.04 nvidia-smi
```

You should see your GPU listed with CUDA version information.

### 2. Start the Stack

```bash
# Navigate to the example directory
cd examples/crush-vllm

# Pull and start services
docker compose up -d
```

This will:

- Start vLLM with GPU support
- Start Olla proxy configured for vLLM
- Download the model (first run only)

### 3. Wait for Model Loading

vLLM needs time to load the model into GPU memory:

```bash
# Monitor vLLM logs
docker logs vllm -f

# Wait for the server to report it is ready to accept requests
```

Model loading times:

- 3B model: ~15-30 seconds
- 7B model: ~30-60 seconds
- 13B model: ~60-120 seconds

### 4. Verify Services

```bash
# Check Olla health
curl http://localhost:40114/internal/health

# Check vLLM health
curl http://localhost:8000/health

# List available models via Olla (OpenAI format)
curl http://localhost:40114/olla/openai/v1/models

# List models via the Anthropic endpoint
curl http://localhost:40114/olla/anthropic/v1/models
```

### 5. Configure Crush CLI

Crush reads its global config from `~/.config/crush/crush.json`. You can also drop a
`crush.json` (or `.crush.json`) in your project directory for project-specific config.

```bash
# Copy the example configuration to the global config location
mkdir -p ~/.config/crush
cp crush-config.example.json ~/.config/crush/crush.json

# Edit the file to match your setup
# The example defines both an openai-compat and an anthropic provider
```

The example config defines two custom providers (`olla-openai` and `olla-anthropic`)
and selects the large/small models via the top-level `models` block. Both `api_key`
values are placeholders; Crush requires a non-empty key for custom providers, but Olla
does not validate it for local use.

### 6. Use Crush CLI

```bash
# Launch the interactive TUI (uses the large model from the config)
crush

# Run a single non-interactive prompt
crush run "Explain async/await in Python"

# Pick a specific provider/model for a run
crush run -m olla-openai/meta-llama/Meta-Llama-3.1-8B-Instruct "Write a haiku about GPUs"

# Use the Anthropic provider instead
crush run -m olla-anthropic/meta-llama/Meta-Llama-3.1-8B-Instruct "Write a haiku about GPUs"

# List models known to the configured providers
crush models

# View logs
crush logs -f
```

Inside the interactive TUI you can switch models with the in-app model picker. To change
the default model permanently, edit the top-level `models` block in your `crush.json`.

## Configuration Files

### compose.yaml

Defines the Docker services:

- **vLLM**: High-performance inference engine with GPU support
- **Olla**: Proxy and load balancer with API translation

### olla.yaml

Olla configuration:

- High-performance engine (`olla`) with the streaming profile
- vLLM endpoint discovery and health checks
- Anthropic translator enabled for dual API support

### crush-config.example.json

Crush CLI configuration showing:

- An `olla-openai` provider (`type: openai-compat`) and an `olla-anthropic` provider (`type: anthropic`)
- A `models` array per provider using catwalk model fields (`id`, `name`, `context_window`, `default_max_tokens`, `cost_per_1m_in`, `cost_per_1m_out`)
- A top-level `models` block selecting the default `large` and `small` models

## Model Configuration

### Changing the Model

Edit `compose.yaml` and update the vLLM model parameter:

```yaml
services:
  vllm:
    command:
      - "--model"
      - "meta-llama/Meta-Llama-3.1-8B-Instruct"  # Change this
```

Popular models for vLLM:

- `meta-llama/Meta-Llama-3.1-8B-Instruct` (Recommended, 8B params)
- `meta-llama/Meta-Llama-3.2-3B-Instruct` (Smaller, 3B params)
- `mistralai/Mistral-7B-Instruct-v0.3` (7B params)
- `Qwen/Qwen2.5-7B-Instruct` (7B params)

If you change the vLLM model, update the model `id` values in `crush-config.example.json`
and the top-level `models` block to match.

### GPU Memory Requirements

| Model Size | VRAM Required | Recommended GPU |
|------------|---------------|-----------------|
| 3B | 6-8 GB | RTX 3060 12GB |
| 7B-8B | 12-16 GB | RTX 3090, RTX 4080 |
| 13B | 24-32 GB | RTX 4090, A5000 |
| 30B+ | 48+ GB | A100, H100 |

## Comparing API Formats

Both providers route to the same vLLM backend through Olla. The difference is purely the
API format used between Crush and Olla.

```bash
# OpenAI-compatible provider
crush run -m olla-openai/meta-llama/Meta-Llama-3.1-8B-Instruct "Explain async/await in Python"

# Anthropic provider (same backend through Olla's translation/passthrough layer)
crush run -m olla-anthropic/meta-llama/Meta-Llama-3.1-8B-Instruct "Explain async/await in Python"
```

## Monitoring

### Check Olla Status

```bash
# Overall health
curl http://localhost:40114/internal/health

# Endpoint status
curl http://localhost:40114/internal/status/endpoints

# Model status
curl http://localhost:40114/internal/status/models
```

### Monitor vLLM Performance

```bash
# Prometheus metrics
curl http://localhost:8000/metrics

# Key metrics to watch:
# - vllm:num_requests_running
# - vllm:num_requests_waiting
# - vllm:gpu_cache_usage_perc
# - vllm:time_to_first_token_seconds
```

### View Logs

```bash
# Olla logs
docker logs olla -f

# vLLM logs
docker logs vllm -f

# Combined logs
docker compose logs -f

# Crush logs
crush logs -f
```

## Advanced Configuration

### Multiple vLLM Instances

To add load balancing across multiple vLLM instances, edit `olla.yaml`:

```yaml
discovery:
  static:
    endpoints:
      # Primary vLLM instance (high priority)
      - url: "http://vllm:8000"
        name: "vllm-primary"
        type: "vllm"
        priority: 100

      # Secondary vLLM instance (medium priority)
      - url: "http://vllm-secondary:8000"
        name: "vllm-secondary"
        type: "vllm"
        priority: 75
```

Then add the secondary service to `compose.yaml`.

### Optimise vLLM Performance

Edit the vLLM command in `compose.yaml`:

```yaml
command:
  - "--model"
  - "meta-llama/Meta-Llama-3.1-8B-Instruct"
  - "--max-model-len"
  - "8192"                    # Adjust context window
  - "--gpu-memory-utilization"
  - "0.95"                    # Use 95% of GPU memory
  - "--max-num-seqs"
  - "256"                     # Increase concurrent sequences
  - "--enable-prefix-caching" # Enable prompt caching
```

## Performance Tuning

### For Maximum Throughput

```yaml
# olla.yaml
proxy:
  engine: "olla"              # High-performance engine
  stream_buffer_size: 16384   # Larger buffer

# vLLM command
  - "--max-num-batched-tokens"
  - "8192"                    # Increase batch size
```

### For Low Latency

```yaml
# olla.yaml
proxy:
  engine: "olla"
  connection_timeout: 10s

# vLLM command
  - "--max-num-seqs"
  - "32"                      # Reduce concurrency
```

### For Memory-Constrained GPUs

```yaml
# vLLM command
  - "--gpu-memory-utilization"
  - "0.85"                    # Reduce GPU memory usage
  - "--max-model-len"
  - "4096"                    # Smaller context window
  - "--enforce-eager"         # Disable CUDA graphs (saves memory)
```

## Troubleshooting

### vLLM Out of Memory

**Symptoms**: Container crashes with CUDA out of memory error

**Solutions**:

1. Reduce model size (use 3B instead of 7B)
2. Decrease `--gpu-memory-utilization` to `0.8`
3. Reduce `--max-model-len` to `4096`
4. Add `--enforce-eager` to disable CUDA graphs

### Slow Model Loading

**Symptoms**: vLLM takes minutes to start

**Causes**:

- Large model download (first run)
- Model quantisation/compilation
- Insufficient GPU memory causing swapping

**Solutions**:

1. Pre-download models: `docker compose pull`
2. Use smaller models for testing
3. Check GPU memory: `nvidia-smi`

### Crush Can't Connect

**Symptoms**: Connection refused or timeout errors

**Solutions**:
```bash
# Verify Olla is running
curl http://localhost:40114/internal/health

# Check Olla logs
docker logs olla

# Verify vLLM is healthy
curl http://localhost:8000/health

# Test the full chain
curl -X POST http://localhost:40114/olla/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"meta-llama/Meta-Llama-3.1-8B-Instruct","messages":[{"role":"user","content":"test"}],"max_tokens":10}'
```

### Provider Not Found

**Symptoms**: Crush does not show the configured provider or model

**Solutions**:

1. Check the config file location: `~/.config/crush/crush.json`
2. Validate JSON syntax: `cat ~/.config/crush/crush.json | jq`
3. Ensure each provider has a non-empty `models` array (a custom provider with no models is dropped at load)
4. Ensure each provider has a non-empty `api_key`
5. Review Crush logs: `crush logs`

### Streaming Issues

**Symptoms**: Responses appear all at once instead of streaming

**Solutions**:

1. Ensure Olla is using the streaming profile (`proxy.profile: "streaming"`)
2. Check vLLM is healthy: `curl http://localhost:8000/health`
3. Test streaming directly:
```bash
curl -X POST http://localhost:40114/olla/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"meta-llama/Meta-Llama-3.1-8B-Instruct","messages":[{"role":"user","content":"Count to 10"}],"stream":true}'
```

## Testing

Run the included test script to verify the setup:

```bash
# Make executable
chmod +x test.sh

# Run tests
./test.sh
```

The test script checks:

1. vLLM health endpoint
2. Olla health endpoint
3. Endpoint discovery through Olla
4. Model discovery through both APIs
5. OpenAI format completions
6. Anthropic format messages
7. Streaming functionality

## Next Steps

- **[vLLM Backend Integration](../../docs/content/integrations/backend/vllm.md)** - Full vLLM configuration guide
- **[Anthropic API Reference](../../docs/content/api-reference/anthropic.md)** - API documentation
- **[Olla Configuration Reference](../../docs/content/configuration/reference.md)** - All configuration options

## Related Examples

- [Claude Code + Ollama](../claude-code-ollama/) - Claude Code with Ollama backend
- [Claude Code + llama.cpp](../claude-code-llamacpp/) - Lightweight llama.cpp backend
- [OpenCode + LM Studio](../opencode-lmstudio/) - OpenCode integration

## Support

For issues with:
- **Olla**: [GitHub Issues](https://github.com/thushan/olla/issues)
- **vLLM**: [vLLM GitHub](https://github.com/vllm-project/vllm/issues)
- **Crush CLI**: [Crush GitHub](https://github.com/charmbracelet/crush/issues)
