# Open WebUI + Olla Integration Example

This example runs [Open WebUI](https://github.com/open-webui/open-webui) with Olla as a proxy and load balancer across one or more Ollama instances.

You can also expose an OpenAI-compatible endpoint alongside the Ollama one, which lets you use vLLM, SGLang, or any other OpenAI-compatible backend through the same stack.

## Architecture

```
┌─────────────┐    ┌──────────────┐    ┌─────────────────┐
│  Open WebUI │───▶│     Olla     │───▶│ Ollama Instance │
│ (Port 3000) │    │ (Port 40114) │    │   (External)    │
└─────────────┘    └──────────────┘    └─────────────────┘
                          │
                          ├──────▶┌─────────────────┐
                          │       │ Ollama Instance │
                          │       │   (External)    │
                          │       └─────────────────┘
                          │
                          └──────▶┌─────────────────┐
                                  │ Ollama Instance │
                                  │   (External)    │
                                  └─────────────────┘
```

## Quick Start

> [!NOTE]
> Olla runs inside the container under `/app`. The config file is mounted to
> `/app/config.yaml` and logs are written to `/app/logs/`.

1. **Edit `olla.yaml`** - add your Ollama server URLs under `discovery.static.endpoints`:
   ```yaml
   discovery:
     static:
       endpoints:
         - url: "http://192.168.1.100:11434"
           name: "my-ollama"
           type: "ollama"
           priority: 100
           model_url: "/api/tags"
           health_check_url: "/"
           check_interval: 2s
           check_timeout: 1s
   ```

2. **Start the stack**:
   ```bash
   docker compose up -d
   ```

3. **Access Open WebUI** at http://localhost:3000

## How It Works

The Docker Compose stack runs two services:

- **Olla** - receives requests from Open WebUI and load balances them across your Ollama servers.
- **Open WebUI** - the chat interface, configured to talk to Olla instead of Ollama directly.

Open WebUI connects to Olla over the internal Docker network (`olla-network`) using the service name `olla`.

### Connection endpoints

| Protocol | Open WebUI env var | Olla URL |
|---|---|---|
| Ollama API | `OLLAMA_BASE_URL` | `http://olla:40114/olla/ollama` |
| OpenAI-compatible | `OPENAI_API_BASE_URL` | `http://olla:40114/olla/proxy/v1` |

`OLLAMA_BASE_URL` does not include a path suffix - Open WebUI appends `/api/*` paths itself.
`OPENAI_API_BASE_URL` must include `/v1` - Open WebUI forwards it verbatim.

The default `compose.yaml` uses the Ollama endpoint. Uncomment the `OPENAI_API_BASE_URL` lines to also enable the OpenAI-compatible connection.

## Adding Ollama Instances

Edit `olla.yaml` and add entries under `discovery.static.endpoints`:

```yaml
discovery:
  static:
    endpoints:
      # High-priority local instance (running on Docker host)
      - url: "http://host.docker.internal:11434"
        name: "local-ollama"
        type: "ollama"
        priority: 100
        model_url: "/api/tags"
        health_check_url: "/"
        check_interval: 2s
        check_timeout: 1s

      # Medium-priority remote GPU server
      - url: "http://gpu-server.local:11434"
        name: "gpu-server"
        type: "ollama"
        priority: 75
        model_url: "/api/tags"
        health_check_url: "/"
        check_interval: 2s
        check_timeout: 1s

      # Low-priority backup instance
      - url: "http://backup.local:11434"
        name: "backup-ollama"
        type: "ollama"
        priority: 25
        model_url: "/api/tags"
        health_check_url: "/"
        check_interval: 2s
        check_timeout: 1s
```

## Monitoring

### Check Olla Health
```bash
curl http://localhost:40114/internal/health
```

### Check Endpoint Status
```bash
curl http://localhost:40114/internal/status/endpoints
```

### List All Available Models
```bash
curl http://localhost:40114/olla/models
```

### View Models via Ollama API (what Open WebUI sees)
```bash
curl http://localhost:40114/olla/ollama/api/tags
```

## Troubleshooting

### Open WebUI Can't Connect to Models

1. **Check Olla health**:
   ```bash
   docker logs olla
   curl http://localhost:40114/internal/health
   ```

2. **Verify endpoint configuration**:
   ```bash
   curl http://localhost:40114/internal/status/endpoints
   ```

3. **Check if models are discovered**:
   ```bash
   curl http://localhost:40114/olla/ollama/api/tags
   ```

### Ollama Endpoints Not Healthy

1. **Verify Ollama instances are accessible**:
   ```bash
   curl http://your-ollama-host:11434/
   ```

2. **Check Docker networking**:
   - Use `host.docker.internal` for Ollama running on the Docker host.
   - Use actual IP addresses for remote instances.
   - Ensure firewall rules allow connections on port 11434.

3. **Review Olla logs**:
   ```bash
   docker logs olla -f
   ```

### Performance Issues

1. **Adjust timeouts for very long responses**:
   ```yaml
   proxy:
     connection_timeout: 60s
     response_timeout: 1200s  # 20 minutes for long generations
   ```

## Environment Variables

You can override Olla config using environment variables in `compose.yaml`:

```yaml
services:
  olla:
    environment:
      - OLLA_SERVER_HOST=0.0.0.0
      - OLLA_SERVER_PORT=40114
      - OLLA_PROXY_ENGINE=olla
      - OLLA_LOGGING_LEVEL=info
```

## Advanced Configuration

### Open WebUI with Both Ollama and OpenAI-Compatible Endpoints

```yaml
services:
  openwebui:
    environment:
      # Ollama API (no /v1 suffix - Open WebUI appends its own paths)
      - OLLAMA_BASE_URL=http://olla:40114/olla/ollama
      # OpenAI-compatible API (must include /v1)
      - OPENAI_API_BASE_URL=http://olla:40114/olla/proxy/v1
      - OPENAI_API_KEY=olla
```

### GPU Support for a Local Ollama Instance

If you want to run a local Ollama instance alongside the stack:

```yaml
services:
  ollama:
    image: ollama/ollama:latest
    container_name: ollama
    restart: unless-stopped
    ports:
      - "11434:11434"
    volumes:
      - ollama_data:/root/.ollama
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: 1
              capabilities: [gpu]

  olla:
    # ... existing config
    depends_on:
      - ollama
```

Then set the Ollama endpoint URL in `olla.yaml` to `http://ollama:11434`.

## Support

- **Olla**: [github.com/thushan/olla](https://github.com/thushan/olla)
- **Open WebUI**: [github.com/open-webui/open-webui](https://github.com/open-webui/open-webui)
- **Ollama**: [github.com/ollama/ollama](https://github.com/ollama/ollama)
