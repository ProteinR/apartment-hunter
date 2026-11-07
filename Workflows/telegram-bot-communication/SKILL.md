---
name: telegram-bot-communication
description: Guide on how to find, configure, and communicate with the Apartment Hunter Telegram bot, including toggling debug/MCP modes, and managing user preferences via natural language.
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
TELEGRAM_MCP_AUTH_TOKEN='...' node Workflows/telegram-bot-communication/scripts/telegram_mcp.mjs tools/call send_message '{"chat_id":"Apartmenthuner_bot","message":"/mcp on"}'
```

The helper performs MCP `initialize`, sends `notifications/initialized`, supports JSON and SSE responses, and redacts token-like values in output.

## Bot Modes

### 1. Regular Mode (Default)
In regular mode, any text message sent by the user is treated as a natural language search query. The bot processes it and presents a confirmation flow:
- **Save**: Saves the parsed preferences to the database.
- **Edit**: Prompts the user to send an updated query.
- **Edit Area**: Displays the current geofence GeoJSON for editing.
- **Cancel**: Aborts the operation.

### 2. MCP Mode (Conversational Agent)
Activate MCP Mode to enable a conversational agent powered by LLM function calling.
- **Toggle ON**: Send `/mcp on` or `/mcp` (toggle).
- **Toggle OFF**: Send `/mcp off` or `/mcp` (toggle).
- **Reset History**: Send `/mcp clear` or `/mcp reset`.
- **How it works**:
  - The bot maintains a **sliding context window** of the last 10 messages (persisted in the `profiles.mcp_history` database column).
  - Users can discuss preferences in natural language (e.g. *"Убери духовку из удобств"* or *"Какие у меня сейчас фильтры?"*).
  - The agent updates the database preferences using the `update_preferences` function call or toggles filtering using `set_filtering_status`.

### 3. Debug Mode
Controls notification frequency for processed listings:
- **Toggle**: Send `/debug`, `/debug on`, or `/debug off`.
- **On**: Sends a notification report for *every* processed channel message (except obvious non-listings).
- **Off**: Sends reports only for listings that pass all filters.

## Useful Bot Commands

- `/start` or `/preferences` - Begin/restart step-by-step preference setup.
- `/city`, `/city tbilisi`, `/city batumi` - Show or switch the profile's search city; switching rebuilds the search area.
- `/channels` - Show the list of connected Telegram channel subscriptions.
- `/subscribe @channel_username` - Subscribe to a source.
- `/unsubscribe @channel_username` - Unsubscribe from a source.

## Database Verification

To check active profiles, preferences, and MCP history directly in Postgres:

```sql
-- View all profiles, preferences, and MCP history
SELECT id, preferences, mcp_history FROM profiles;

-- Clear MCP conversation history manually for a user
UPDATE profiles SET mcp_history = '[]' WHERE id = 'telegram_<chat_id>';
```
