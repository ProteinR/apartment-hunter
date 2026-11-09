---
name: telegram-bot-communication
description: Guide on how to find, configure, and communicate with the Apartment Hunter Telegram bot, and managing the whole search profile through conversation.
---

# Telegram Bot Communication

Use this skill when you need to understand how the Telegram bot works, how to find it, how to configure it, and how to communicate with it to manage user preferences.

## Bot Details

- **Bot Username**: `@Apartmenthuner_bot` *(Note: Spelled without the middle 't' in hunter)*
- **Configuration**: Configured via the `TELEGRAM_BOT_TOKEN` environment variable in the `.env` file.
- **Running the Bot**: Run the bot locally using `go run ./cmd/apartment-hunter telegram` or via Docker Compose (`docker compose up bot`).

## Telegram MCP

- **Endpoint**: `https://tg-mcp.memruk.dev/mcp`
- **Authentication**: `Authorization: Bearer <token>`. Prefer `TELEGRAM_MCP_AUTH_TOKEN` in the current environment; never commit or persist the token in repo files.
- **Helper**:

```bash
TELEGRAM_MCP_AUTH_TOKEN='...' node Workflows/telegram-bot-communication/scripts/telegram_mcp.mjs tools/list
TELEGRAM_MCP_AUTH_TOKEN='...' node Workflows/telegram-bot-communication/scripts/telegram_mcp.mjs tools/call get_me
TELEGRAM_MCP_AUTH_TOKEN='...' node Workflows/telegram-bot-communication/scripts/telegram_mcp.mjs tools/call send_message '{"chat_id":"Apartmenthuner_bot","message":"/start"}'
```

The helper performs MCP `initialize`, sends `notifications/initialized`, supports JSON and SSE responses, and redacts token-like values in output.

## Conversation

The bot has no reply keyboard and no modes. Every text message goes to the Claude agent (`internal/adapters/mcpagent`), which configures the profile through tools. The Telegram menu lists only `/start` and `/reset`.

- `/start` (also `/preferences`, `/restart`): creates the profile if needed and sends a greeting without an LLM call. A profile without preferences gets the onboarding greeting; the agent then asks one or two questions per message (city, budget, size, districts, must-haves, nice-to-haves, dealbreakers) and calls `update_preferences` only after the user confirms a summary. The greeting turn is appended to history; `/start` does not erase it.
- `/reset` (also `/clear`): clears `profiles.mcp_history`, including the summary; filters stay.
- Sources: the agent calls `list_sources`, `add_sources`, `remove_sources` (implemented by the bot with the same userbot membership check as `/subscribe`). A pasted `@name` or `t.me/name` goes to the agent too.
- Notifications: `set_notification_mode` with `matches_only` or `all_reports` sets `profiles.debug_mode`.
- Replying to a listing report is feedback and goes to `ProcessFeedback`.
- History: up to 40 recent messages are sent verbatim. Above that, older messages and messages older than `MCP_CONTEXT_TTL_HOURS` (default 24) are folded by one extra LLM call into a `summary` entry stored in `mcp_history` and added to the system prompt. If summarization fails, history is kept up to 80 messages.
- `/mcp` only explains that a separate mode is no longer needed.
- Access: strangers get an invitation notice and the admins in `TELEGRAM_ADMIN_CHAT_IDS` (production: only @dm_sg; the userbot account is not an admin) receive Allow/Deny buttons. The admin can also write «открой доступ @name», «закрой доступ @name», «кто пользуется ботом».

## Hidden Legacy Commands

They still work but are not shown anywhere:

- `/city`, `/city tbilisi`, `/city batumi` - Show or switch the profile's search city; switching rebuilds the search area.
- `/channels`, `/subscribe @channel_username`, `/unsubscribe @channel_username` - Manage sources.
- `/debug`, `/debug on`, `/debug off` - Notification volume.

Without an agent (no LLM key) `/start` falls back to the old one-shot wizard with Save/Edit/Edit area/Cancel.

## Database Verification

To check active profiles, preferences, and MCP history directly in Postgres:

```sql
-- View all profiles, preferences, and MCP history
SELECT id, preferences, mcp_history FROM profiles;

-- Clear MCP conversation history manually for a user
UPDATE profiles SET mcp_history = '[]' WHERE id = 'telegram_<chat_id>';
```
