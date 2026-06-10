# OpenCode + LM Studio Integration Example

This example sets up [OpenCode](https://opencode.ai/) with Olla as a proxy to
LM Studio for local AI coding assistance. OpenCode talks to Olla's
OpenAI-compatible endpoint, and Olla proxies to LM Studio running on your host
machine.

## Architecture

```
┌─────────────┐    ┌──────────┐    ┌─────────────────┐
│  OpenCode   │───▶│   Olla   │───▶│   LM Studio     │
│   (Local)   │    │(Container│    │   (Host)        │
│             │    │ :40114)  │    │   :1234         │
└─────────────┘    └──────────┘    └─────────────────┘
```

OpenCode connects to Olla's OpenAI-compatible endpoint via the
`@ai-sdk/openai-compatible` provider:

```
http://localhost:40114/olla/openai/v1
```

This is a direct passthrough to LM Studio with no translation overhead.

> **Why not the Anthropic endpoint?** OpenCode's `@ai-sdk/anthropic` provider
> has a known bug where a custom `baseURL` causes request keys to be dropped
> (opencode issue #21737). For that reason this example uses
> `@ai-sdk/openai-compatible` against Olla's OpenAI endpoint. Olla's
> `/olla/anthropic/v1` endpoint still works for other clients (and LM Studio
> has native Anthropic support, so those requests are passed through rather
> than translated).

## Prerequisites

1. **LM Studio** installed and running on your host machine
   - Download from: https://lmstudio.ai/
   - Enable the server (Developer -> Server -> Start Server)
   - Default port: 1234
   - Load a model (e.g. Qwen, Llama, Mistral)

2. **Docker** installed for running Olla

3. **OpenCode** installed:
   ```bash
   curl -fsSL https://opencode.ai/install | bash
   # or
   npm i -g opencode-ai
   ```

## Quick Start

### 1. Start LM Studio

1. Open LM Studio.
2. Download and load a model (e.g. `qwen2.5-coder-7b-instruct`).
3. Go to Developer -> Server.
4. Click "Start Server" (default: http://localhost:1234).
5. Verify it is running:
   ```bash
   curl http://localhost:1234/v1/models
   ```

### 2. Configure Olla

The `olla.yaml` configuration is already set up to reach LM Studio on your host.

**For Windows/Mac** (using `host.docker.internal`):
```yaml
endpoints:
  - url: "http://host.docker.internal:1234"
    name: "lm-studio"
    type: "lm-studio"
```

**For Linux** (using the Docker bridge IP):
```yaml
endpoints:
  - url: "http://172.17.0.1:1234"  # default Docker bridge IP
    name: "lm-studio"
    type: "lm-studio"
```

Edit `olla.yaml` if your setup differs.

### 3. Start Olla

```bash
docker compose up -d
```

### 4. Verify connectivity

```bash
# Check Olla health
curl http://localhost:40114/internal/health

# Check the LM Studio endpoint status
curl http://localhost:40114/internal/status/endpoints

# List models via the OpenAI endpoint
curl http://localhost:40114/olla/openai/v1/models
```

Note the model ids in the `/olla/openai/v1/models` response. You will need them
for the OpenCode `models` map in the next step.

### 5. Configure OpenCode

OpenCode reads its global config from `~/.config/opencode/opencode.json`
(per-project config is `opencode.json` or `opencode.jsonc` in the project root).

```bash
# Linux/Mac
mkdir -p ~/.config/opencode
cp opencode-config.example.json ~/.config/opencode/opencode.json

# Windows (PowerShell)
New-Item -ItemType Directory -Force -Path $env:USERPROFILE\.config\opencode
Copy-Item opencode-config.example.json $env:USERPROFILE\.config\opencode\opencode.json
```

The example configuration looks like this:

```json
{
  "$schema": "https://opencode.ai/config.json",
  "model": "olla-openai/qwen2.5-coder-7b-instruct",
  "provider": {
    "olla-openai": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Olla (LM Studio via OpenAI)",
      "options": {
        "baseURL": "http://localhost:40114/olla/openai/v1"
      },
      "models": {
        "qwen2.5-coder-7b-instruct": {
          "name": "Qwen2.5 Coder 7B Instruct"
        }
      }
    }
  }
}
```

Important points:

- The keys inside `models` **must match model ids returned by**
  `curl http://localhost:40114/olla/openai/v1/models`. OpenCode only exposes the
  models you list here, so replace `qwen2.5-coder-7b-instruct` with the id of
  the model you have loaded in LM Studio.
- The top-level `model` field selects the default model in the form
  `provider/model-id` (here `olla-openai/qwen2.5-coder-7b-instruct`). The
  provider key (`olla-openai`) is the name under `provider` above.
- OpenCode validates this file against its schema, so do not add unknown
  top-level keys (for example, there is no `notes` field).

### 6. Use OpenCode

```bash
# Start OpenCode (uses the default "model" from the config)
opencode

# Or pick a model for a single run (format: provider/model-id)
opencode run -m olla-openai/qwen2.5-coder-7b-instruct "Refactor this function"
```

To switch models interactively inside the TUI, run the `/models` command.
There is no `--provider` flag; selection is by model in `provider/model-id`
form.

## Networking Notes

### Windows/Mac

Use `host.docker.internal` to reach services on your host from Docker. This is
already set in the provided `olla.yaml`.

### Linux

Docker on Linux does not provide `host.docker.internal` by default. Use one of:

**Option 1: Docker bridge IP** (already commented in `olla.yaml`):
```yaml
- url: "http://172.17.0.1:1234"
```

**Option 2: Add a host-gateway mapping** to `compose.yaml`:
```yaml
services:
  olla:
    extra_hosts:
      - "host.docker.internal:host-gateway"
```
Then use `http://host.docker.internal:1234` in `olla.yaml`.

## Testing

Run the included test script:

```bash
chmod +x test.sh
./test.sh
```

The script checks LM Studio connectivity, the Olla container, the Olla health
endpoint, the endpoint status, the OpenAI `/models` endpoint, and a chat
completion request.

## Troubleshooting

### LM Studio not reachable

**Symptom**: the LM Studio endpoint shows as unhealthy in Olla.

1. Verify the LM Studio server is running (Developer -> Server) and reachable
   from the host: `curl http://localhost:1234/v1/models`.
2. Check that port 1234 is not blocked by a firewall.
3. Linux users: confirm the Docker bridge IP with
   `ip addr show docker0 | grep inet` and update `olla.yaml` if it differs from
   `172.17.0.1`.
4. Mac/Windows users: ensure Docker Desktop is recent enough for
   `host.docker.internal`.

### Models not appearing

**Symptom**: `curl http://localhost:40114/olla/openai/v1/models` returns an
empty list.

1. Load a model in LM Studio (models must be loaded, not just downloaded).
2. Check endpoint health:
   `curl http://localhost:40114/internal/status/endpoints`
3. Wait for discovery (runs every 5 minutes) or restart Olla:
   `docker restart olla`

If models appear at Olla but not in OpenCode, make sure the `models` map in
`opencode.json` lists the matching model ids.

### OpenCode connection issues

1. Check the config file:
   ```bash
   # Linux/Mac
   cat ~/.config/opencode/opencode.json
   # Windows
   type %USERPROFILE%\.config\opencode\opencode.json
   ```
2. Verify Olla is reachable from the host:
   `curl http://localhost:40114/olla/openai/v1/models`
3. Test a chat completion with curl first:
   ```bash
   curl http://localhost:40114/olla/openai/v1/chat/completions \
     -H "Content-Type: application/json" \
     -d '{
       "model": "qwen2.5-coder-7b-instruct",
       "messages": [{"role": "user", "content": "Hello!"}]
     }'
   ```

### Docker issues

```bash
# Check logs
docker logs olla

# Check the port is free
netstat -an | grep 40114    # Linux/Mac
netstat -an | findstr 40114 # Windows

# Restart with fresh state
docker compose down
docker compose up -d
```

## Adjusting Timeouts

For large models with slow generation, raise the proxy timeouts in `olla.yaml`:

```yaml
proxy:
  response_timeout: 1800s  # 30 minutes
  read_timeout: 600s       # 10 minutes
```

## Enabling Debug Logging

```yaml
logging:
  level: "debug"
```

Then follow the logs: `docker logs olla -f`.

## Related Examples

- [Claude Code + Ollama](../claude-code-ollama/) - similar setup with Claude Code

## Support

- **Olla**: [Olla GitHub repository](https://github.com/thushan/olla)
- **OpenCode**: https://opencode.ai/
- **LM Studio**: https://lmstudio.ai/

## Licence

This example is part of the Olla project and follows the same licence.
