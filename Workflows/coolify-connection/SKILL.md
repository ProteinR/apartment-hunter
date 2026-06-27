---
name: coolify-connection
description: Connect to the Coolify MCP endpoint for the apartment-hunter production deployment. Use when Codex needs to inspect Coolify infrastructure, project/application/database status, available Coolify MCP tools, or diagnose production availability/log access for apartment-hunter via https://coolify.memruk.dev/mcp.
---

# Coolify Connection

## Overview

Use Coolify MCP for read-only production infrastructure checks. The endpoint is:

```text
https://coolify.memruk.dev/mcp
```

Authenticate with `Authorization: Bearer <token>` using a token created in Coolify: Security -> API Tokens. Never commit or persist the token in repo files, skill files, logs, or generated scripts.

## Token Handling

Prefer a token supplied by the current user request. Otherwise look for `COOLIFY_MCP_TOKEN` in the environment.

If Coolify returns `401`, `403`, or an auth-related JSON-RPC error, do not keep retrying with the same token. Tell the user the Coolify token may be expired or invalid and ask them to regenerate an API token in Coolify Security -> API Tokens.

If a Python HTTP client receives Cloudflare `Error 1010` / `browser_signature_banned`, retry with Node `fetch` before concluding the token is bad. This endpoint has accepted Node `fetch` when Python was blocked by Cloudflare.

## Helper Script

Use `scripts/coolify_mcp.mjs` for deterministic MCP calls:

```bash
COOLIFY_MCP_TOKEN='...' node Workflows/coolify-connection/scripts/coolify_mcp.mjs tools/list
COOLIFY_MCP_TOKEN='...' node Workflows/coolify-connection/scripts/coolify_mcp.mjs tools/call get_infrastructure_overview
COOLIFY_MCP_TOKEN='...' node Workflows/coolify-connection/scripts/coolify_mcp.mjs tools/call list_applications '{"per_page":100}'
```

The script performs `initialize`, sends `notifications/initialized`, then calls the requested JSON-RPC method with the returned `Mcp-Session-Id`.

## Expected Coolify Scope

For apartment-hunter, the production resources known at creation time were:

- Project: `Apartment-hunter bot`
- Application: `apartment-hunter`
- Database: `apartment-hunter-db`

Start with `get_infrastructure_overview`, then use `list_applications`, `list_databases`, or specific `get_*` tools as needed.

## Logs

The current Coolify MCP server may expose only inventory tools, not container log tools. If `tools/list` has no log-reading tool, state that Coolify MCP cannot fetch logs directly.

For production debugging:

- Use MCP to confirm app/database status and discover available tools.
- Use Coolify UI/API/SSH or a future MCP log tool for container stdout/stderr.
- For detailed apartment-hunter listing decisions, inspect the JSONL log directories configured in the app env:
  - `APARTMENT_HUNTER_PIPELINE_LOG_DIR`
  - `APARTMENT_HUNTER_GEOCODING_LOG_DIR`
- In this repo's compose defaults, persistent JSONL logs are under `/app/.telegram/logs/...`.

Avoid requesting full `get_application` or `get_database` output unless needed, because those responses may include sensitive environment/configuration details. Redact secrets before showing any output to the user.
