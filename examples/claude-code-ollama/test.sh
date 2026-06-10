#!/bin/bash
# Test script for Claude Code + Ollama + Olla setup

set -e  # Exit on error

OLLA_URL="http://localhost:40114"
ANTHROPIC_URL="${OLLA_URL}/olla/anthropic/v1"

echo "Testing Olla + Ollama + Anthropic Translation..."
echo ""

# Colours for output
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Test 1: Health check
echo -e "${YELLOW}Test 1: Checking Olla health...${NC}"
if curl -sf "${OLLA_URL}/internal/health" > /dev/null; then
    echo -e "${GREEN}  Olla is healthy${NC}"
else
    echo -e "${RED}  Olla health check failed${NC}"
    exit 1
fi
echo ""

# Test 2: List models
echo -e "${YELLOW}Test 2: Listing available models...${NC}"
MODELS=$(curl -s "${ANTHROPIC_URL}/models")
if echo "$MODELS" | jq -e '.data | length > 0' > /dev/null 2>&1; then
    echo -e "${GREEN}  Models available:${NC}"
    echo "$MODELS" | jq -r '.data[].id' | sed 's/^/    - /'
else
    echo -e "${RED}  No models available. Pull a model first:${NC}"
    echo "    docker exec ollama ollama pull llama3.2:latest"
    exit 1
fi
echo ""

# Get first available model
MODEL=$(echo "$MODELS" | jq -r '.data[0].id')
echo -e "${YELLOW}Using model: ${MODEL}${NC}"
echo ""

# Test 3: Simple message (non-streaming)
echo -e "${YELLOW}Test 3: Testing simple message (non-streaming)...${NC}"
RESPONSE=$(curl -s -X POST "${ANTHROPIC_URL}/messages" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d "{
    \"model\": \"${MODEL}\",
    \"max_tokens\": 100,
    \"messages\": [
      {\"role\": \"user\", \"content\": \"Say hello in exactly one sentence.\"}
    ]
  }")

if echo "$RESPONSE" | jq -e '.content[0].text' > /dev/null 2>&1; then
    echo -e "${GREEN}  Non-streaming message successful${NC}"
    echo "  Response:"
    echo "$RESPONSE" | jq -r '.content[0].text' | sed 's/^/    /'

    # Show Olla routing headers
    echo ""
    echo "  Olla routing headers:"
    curl -sI -X POST "${ANTHROPIC_URL}/messages" \
      -H "Content-Type: application/json" \
      -d "{\"model\":\"${MODEL}\",\"max_tokens\":10,\"messages\":[{\"role\":\"user\",\"content\":\"Hi\"}]}" \
      | grep -i "x-olla" | sed 's/^/    /'
else
    echo -e "${RED}  Non-streaming message failed${NC}"
    echo "$RESPONSE" | jq .
    exit 1
fi
echo ""

# Test 4: Streaming message
echo -e "${YELLOW}Test 4: Testing streaming message...${NC}"

STREAM_OUTPUT=$(curl -sN -X POST "${ANTHROPIC_URL}/messages" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d "{
    \"model\": \"${MODEL}\",
    \"max_tokens\": 50,
    \"messages\": [
      {\"role\": \"user\", \"content\": \"Count from 1 to 3, one number per line.\"}
    ],
    \"stream\": true
  }")

if echo "$STREAM_OUTPUT" | grep -q "content_block_delta"; then
    echo -e "${GREEN}  Streaming message successful${NC}"
    echo "  Stream content:"
    echo "$STREAM_OUTPUT" | grep "content_block_delta" | \
      sed 's/data: //' | \
      jq -r '.delta.text // empty' | \
      tr -d '\n' | \
      sed 's/^/    /'
    echo ""
else
    echo -e "${RED}  Streaming message failed${NC}"
    echo "$STREAM_OUTPUT"
    exit 1
fi
echo ""

# Test 5: Endpoint status
echo -e "${YELLOW}Test 5: Checking endpoint status...${NC}"
ENDPOINTS=$(curl -s "${OLLA_URL}/internal/status/endpoints")
if echo "$ENDPOINTS" | jq -e '.endpoints | length > 0' > /dev/null 2>&1; then
    echo -e "${GREEN}  Endpoints registered:${NC}"
    echo "$ENDPOINTS" | jq -r '.endpoints[] | "    - \(.name) (\(.type)) - \(.status)"'
else
    echo -e "${YELLOW}  No endpoints registered${NC}"
fi
echo ""

# Summary
echo -e "${GREEN}================================================${NC}"
echo -e "${GREEN}  All tests passed!${NC}"
echo -e "${GREEN}================================================${NC}"
echo ""
echo "Configure Claude Code:"
echo "  export ANTHROPIC_BASE_URL=\"${OLLA_URL}/olla/anthropic\""
echo "  export ANTHROPIC_AUTH_TOKEN=\"not-required\""
echo ""
echo "Then run: claude"
echo ""
echo "Useful commands:"
echo "  Olla logs:    docker compose logs -f olla"
echo "  Ollama logs:  docker compose logs -f ollama"
echo "  Status:       curl ${OLLA_URL}/internal/status | jq"
echo "  Models:       curl ${ANTHROPIC_URL}/models | jq"
