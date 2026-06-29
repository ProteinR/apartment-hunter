#!/usr/bin/env node

const endpoint = process.env.COOLIFY_MCP_ENDPOINT || "https://coolify.memruk.dev/mcp";

function usage() {
  console.error(`Usage:
  COOLIFY_MCP_TOKEN=... node scripts/coolify_mcp.mjs tools/list
  COOLIFY_MCP_TOKEN=... node scripts/coolify_mcp.mjs tools/call <tool_name> [json_arguments]

Options:
  --token <token>      Use token instead of COOLIFY_MCP_TOKEN
  --endpoint <url>     Use endpoint instead of COOLIFY_MCP_ENDPOINT/default`);
}

function parseArgs(argv) {
  const args = [...argv];
  let token = process.env.COOLIFY_MCP_TOKEN || "";
  let url = endpoint;

  for (let i = 0; i < args.length; ) {
    if (args[i] === "--token") {
      token = args[i + 1] || "";
      args.splice(i, 2);
      continue;
    }
    if (args[i] === "--endpoint") {
      url = args[i + 1] || "";
      args.splice(i, 2);
      continue;
    }
    i += 1;
  }

  return { token, endpoint: url, args };
}

async function post(url, token, payload, sessionId) {
  const headers = {
    Authorization: `Bearer ${token}`,
    "Content-Type": "application/json",
    Accept: "application/json, text/event-stream",
  };
  if (sessionId) {
    headers["Mcp-Session-Id"] = sessionId;
  }

  const response = await fetch(url, {
    method: "POST",
    headers,
    body: JSON.stringify(payload),
  });
  const text = await response.text();
  if (!response.ok) {
    const error = new Error(`HTTP ${response.status}: ${text.slice(0, 1000)}`);
    error.status = response.status;
    throw error;
  }
  return {
    headers: response.headers,
    body: text ? parseMaybeSSE(text) : null,
  };
}

function parseMaybeSSE(text) {
  const line = text.split(/\n/).find((value) => value.startsWith("data: "));
  return JSON.parse(line ? line.slice(6) : text);
}

function redact(value) {
  return JSON.parse(JSON.stringify(value), (key, val) => {
    if (typeof val !== "string") return val;
    if (/^(sk-|4\||[0-9]+:)[A-Za-z0-9:_|.-]{12,}/.test(val)) return "<redacted>";
    if (/token|secret|password|api[_-]?key/i.test(key)) return "<redacted>";
    return val;
  });
}

async function main() {
  const { token, endpoint: url, args } = parseArgs(process.argv.slice(2));
  if (!token || !url || args.length < 1) {
    usage();
    process.exit(2);
  }

  const init = await post(url, token, {
    jsonrpc: "2.0",
    id: 1,
    method: "initialize",
    params: {
      protocolVersion: "2024-11-05",
      capabilities: {},
      clientInfo: { name: "codex-coolify-connection", version: "1.0.0" },
    },
  });

  const sessionId = init.headers.get("mcp-session-id");
  if (!sessionId) {
    throw new Error("MCP initialize response did not include Mcp-Session-Id");
  }

  await post(url, token, {
    jsonrpc: "2.0",
    method: "notifications/initialized",
    params: {},
  }, sessionId);

  let payload;
  if (args[0] === "tools/list") {
    payload = { jsonrpc: "2.0", id: 2, method: "tools/list", params: {} };
  } else if (args[0] === "tools/call") {
    const toolName = args[1];
    if (!toolName) {
      usage();
      process.exit(2);
    }
    let toolArgs = {};
    if (args[2]) {
      toolArgs = JSON.parse(args[2]);
    }
    payload = {
      jsonrpc: "2.0",
      id: 2,
      method: "tools/call",
      params: { name: toolName, arguments: toolArgs },
    };
  } else {
    usage();
    process.exit(2);
  }

  const result = await post(url, token, payload, sessionId);
  console.log(JSON.stringify(redact(result.body), null, 2));
}

main().catch((error) => {
  const status = error.status ? `HTTP ${error.status}` : error.name;
  console.error(`${status}: ${error.message}`);
  if (error.status === 401 || error.status === 403 || /auth|token|unauthorized|forbidden/i.test(error.message)) {
    console.error("Coolify token may be expired or invalid. Ask the user to regenerate a token in Coolify Security -> API Tokens.");
  }
  process.exit(1);
});
