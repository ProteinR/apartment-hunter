# apartment-hunter

A Telegram bot that watches rental channels and chats in Batumi and Tbilisi and sends you only the listings that match your criteria.

You describe what you want in plain language ("2 rooms up to $700 near 6 May Park, with a dishwasher"). The bot turns that into structured preferences and a search area on the map. A Telegram user account (the "userbot") reads the channels you subscribe to. Every new listing goes through cheap text filters, geocoding and a PostGIS location check, then photo deduplication and a vision model that looks at the interior. Matching listings arrive as a photo collage with a score and a criteria checklist.

The bot talks to users in Russian.

## How it works

1. **Bot** (`telegram` command): a conversational agent (Claude, or any OpenAI-compatible model) manages each user's preferences, search area, sources and notification mode.
2. **Worker** (started inside the bot process, or separately with `telegram-process`): one MTProto session of the userbot account listens to new messages in the subscribed chats and catches up on recent history for new subscriptions.
3. **Pipeline** for every message and every subscribed profile:
   - parse the text into a listing (price, area, rooms, address, amenities);
   - drop non-listings and apply price, area and required-amenity filters;
   - geocode the address with Google Maps and check it against the profile's search area in PostGIS;
   - only then download photos, skip duplicates by perceptual image hashes, and ask the vision model about style, renovation, view and amenities;
   - score the listing, save it to `listing_evaluations` and notify the user.

Text and location rejections happen before any photo download or model call, so most messages cost nothing.

## What you need

- **Go 1.25+** (for local runs and tests) or **Docker**.
- **PostgreSQL with PostGIS**. The schema is created automatically at startup (`create extension if not exists postgis`).
- **A Telegram bot token** from [@BotFather](https://t.me/BotFather).
- **A Telegram user account for the userbot** and its API credentials (`api_id`, `api_hash`) from [my.telegram.org](https://my.telegram.org). The bot never joins chats by itself: this account must already be a member of every channel or group you want to watch.
- **A model API key**: `ANTHROPIC_API_KEY` (recommended), or `OPENROUTER_API_KEY` / `OPENAI_API_KEY`.
- **A Google Maps API key** with the Geocoding API and Maps Static API enabled. Restrict the key to these two APIs and set daily quotas.

## Configuration

Copy the example and fill it in:

```bash
cp .env.example .env
```

Required:

| Variable | Purpose |
|---|---|
| `DATABASE_URL` | Postgres connection string. |
| `TELEGRAM_BOT_TOKEN` | Bot token from BotFather. |
| `TELEGRAM_API_ID`, `TELEGRAM_API_HASH` | Userbot API credentials. Without them the bot runs without the worker. |
| `TELEGRAM_SESSION_STRING` or `TELEGRAM_PHONE` | Userbot login: a ready session string, or a phone number for an interactive login. |
| `ANTHROPIC_API_KEY` or `OPENROUTER_API_KEY` / `OPENAI_API_KEY` | Model for the conversational agent, the preferences parser and photo analysis. |
| `APARTMENT_HUNTER_GOOGLE_MAPS_API_KEY` | Geocoding and search-area maps. |

Access control:

| Variable | Purpose |
|---|---|
| `TELEGRAM_ALLOWED_CHAT_IDS`, `TELEGRAM_ALLOWED_USERNAMES` | Comma-separated allowlist. |
| `APARTMENT_HUNTER_PROFILE_ID` | `telegram_<chat id>`. That chat id is added to the allowlist. `docker-compose.yaml` defaults it to `telegram_0`, which already turns the allowlist on. |
| `TELEGRAM_ADMIN_CHAT_IDS` | Admins. The userbot account is always an admin. |

When the allowlist is empty (no allowed ids or usernames and no `APARTMENT_HUNTER_PROFILE_ID`), anyone who finds the bot can use it and spend your model and Maps budget. Otherwise a stranger's first message sends every admin an Allow/Deny request, and admins can grant or revoke access by asking the bot. Set `APARTMENT_HUNTER_PROFILE_ID=telegram_<your chat id>` and put your chat id into `TELEGRAM_ADMIN_CHAT_IDS`, then send `/start` to the bot yourself first: Telegram bots cannot message a user who has never started them, so admin requests would not reach you.

Optional:

| Variable | Default | Purpose |
|---|---|---|
| `ANTHROPIC_MODEL` | `claude-haiku-5-5` | Model for text tasks. |
| `ANTHROPIC_VISION_MODEL` | `ANTHROPIC_MODEL` | Model for photo analysis. |
| `ANTHROPIC_EFFORT` | empty | Reasoning effort passed to Anthropic. |
| `OPENROUTER_BASE_URL`, `OPENAI_BASE_URL` | OpenRouter | OpenAI-compatible endpoint used without Anthropic. The preferences parser and photo analysis default to OpenRouter even when only `OPENAI_API_KEY` is set, so set `OPENAI_BASE_URL=https://api.openai.com/v1` to use OpenAI directly. |
| `OPENROUTER_PREFERENCES_MODEL`, `OPENAI_PREFERENCES_MODEL` | `openai/gpt-4o-mini` (parser), `gpt-5.4-mini` (agent) | Text model without Anthropic. |
| `OPENROUTER_VISION_MODEL`, `OPENAI_VISION_MODEL` | `openai/gpt-4o-mini`; `.env.example` and compose set `gpt-4.1-mini` | Vision model without Anthropic. Use the provider's model id format (`openai/...` on OpenRouter). |
| `*_VISION_*_PRICE_PER_MILLION_USD` | Haiku 5.5 / gpt-4.1-mini prices | Prices used to record the cost of each analysis. |
| `TELEGRAM_PASSWORD` | empty | 2FA password for an interactive userbot login. |
| `TELEGRAM_SESSION_FILE` | `.telegram/userbot.session` | Where an interactive login stores its session. |
| `TELEGRAM_MEDIA_DIR` | `.telegram/media` | Temporary photo downloads; files are deleted after processing. |
| `TELEGRAM_COLLAGE_NOTIFICATIONS` | `true` | Send listings as one photo collage. |
| `TELEGRAM_RICH_NOTIFICATIONS` | `true` | Fallback rich message when the collage fails. |
| `APARTMENT_HUNTER_GEOCODING_REGION` | `Batumi, Georgia` | Region appended to short addresses. |
| `APARTMENT_HUNTER_GEOCODING_LANGUAGE` | `en` | Geocoding result language. |
| `MCP_CONTEXT_TTL_HOURS` | `24` | Conversation history older than this is summarized. |
| `APARTMENT_HUNTER_PIPELINE_LOG_DIR`, `APARTMENT_HUNTER_GEOCODING_LOG_DIR` | `.logs/pipeline`, `.logs/geocoding` | JSONL decision logs. |
| `APARTMENT_HUNTER_PIPELINE_LOG_RETENTION_DAYS`, `APARTMENT_HUNTER_GEOCODING_LOG_RETENTION_DAYS` | `3` | Log retention. |
| `APARTMENT_HUNTER_LOG_TO_STDOUT` | `false` | Also print the JSONL events to stdout. |

## Userbot login

The easiest way is a session string generated once on your machine:

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r scripts/requirements-session.txt
python scripts/session_string_generator.py --qr
```

The script reads `TELEGRAM_API_ID` and `TELEGRAM_API_HASH` from `.env` and logs in by QR code (or `--phone`). Put the printed string into `TELEGRAM_SESSION_STRING`. It must be created with the same API credentials. With a session string, `TELEGRAM_PHONE` and `TELEGRAM_PASSWORD` can stay empty.

Without a session string, set `TELEGRAM_PHONE` and run the worker interactively once: it asks for the login code in the terminal and saves the session to `TELEGRAM_SESSION_FILE`.

Run only one process per userbot session. Two processes with the same session break each other, and Telegram may revoke the session.

## Run with Docker

`docker-compose.yaml` contains the application only and joins an external network named `coolify` (the production setup). For a local run, create that network and start PostGIS on it under the host name `postgres`, which the default `DATABASE_URL` in compose expects:

```bash
docker network create coolify
docker run -d --name postgres --network coolify \
  -e POSTGRES_USER=apartment_hunter \
  -e POSTGRES_PASSWORD=apartment_hunter \
  -e POSTGRES_DB=apartment_hunter \
  -v apartment-hunter-pg:/var/lib/postgresql/data \
  postgis/postgis:16-3.4
```

Leave `DATABASE_URL` out of `.env` (it points at `localhost` for non-Docker runs), or set it to the `postgres` host.

If you log in with `TELEGRAM_PHONE` instead of a session string, do the interactive login first, before the bot starts, because the `bot` container has no terminal to type the code into:

```bash
docker compose run --rm worker
```

Enter the code (and the 2FA password if asked), wait for `Listening for new Telegram messages`, then stop it with Ctrl+C. The session is saved in the `telegram-session` volume. Keep `TELEGRAM_PHONE` set afterwards: the worker starts only when it or `TELEGRAM_SESSION_STRING` is present.

Then start the bot:

```bash
docker compose up --build -d bot
```

The `bot` service runs the bot and, when the userbot variables are set, the worker in the same process. The `telegram-session` volume keeps the userbot session file, temporary media and JSONL logs. Do not run the `worker` service next to `bot`, or every notification arrives twice.

## Run without Docker

Start PostGIS (for example the container above with `-p 5432:5432`), keep the `localhost` `DATABASE_URL` from `.env.example`, then:

```bash
set -a; source .env; set +a
go run ./cmd/apartment-hunter telegram
```

## Using the bot

1. Join the rental channels and groups you want to watch with the userbot account.
2. Open your bot in Telegram and send `/start`.
3. Describe what you are looking for. The bot asks short follow-up questions, shows a summary and saves it after you confirm. It builds the search area from the districts and landmarks you name (a 1.5 km circle by default) and can send you the map.
4. Add sources by sending links or usernames (`https://t.me/example_channel`, `@example_channel`), or ask the bot to suggest sources other users of your city read. The bot refuses usernames the userbot cannot resolve and warns when the userbot is not a member.
5. Change anything later in conversation, or with `/settings` (a settings card with budget, area, dishwasher/oven and districts). `/reset` clears the conversation history and keeps the saved criteria.

By default you get only listings that passed. Ask the bot to show every processed listing (debug mode) when you want to see why things were rejected. Under each listing, the «Не подходит» button marks it as unsuitable, and a text reply to the listing message («too dark», «ok but too far») lets the bot adjust the criteria with you.

Supported cities are Batumi and Tbilisi (`internal/cities`). Batumi has extra shortcuts such as the sea-distance corridor and the Pushkin street boundary.

## CLI commands

```text
apartment-hunter telegram                     bot, plus the worker when userbot variables are set
apartment-hunter telegram-process [profile]   worker only, for all profiles or one
apartment-hunter telegram-read <profile>      print recent messages of a profile's sources as JSON
apartment-hunter score listing.json prefs.yaml  score one listing against a YAML preferences file
apartment-hunter interview [prefs.yaml]       write a YAML preferences file by answering questions
```

`score` uses a mock vision evaluator unless `APARTMENT_HUNTER_VISION=openai` is set. Try it with the bundled examples:

```bash
go run ./cmd/apartment-hunter score examples/sample_listing.json configs/default_preferences.yaml
```

## Data and logs

Postgres tables worth knowing:

- `profiles`: chat id, current preferences, notification mode, conversation history.
- `profile_channels`: subscriptions of each profile.
- `user_search_profiles`: preference snapshots and the PostGIS search area.
- `listing_evaluations`: every processed message with status, score, report, model usage and cost.
- `listing_images`, `listing_duplicates`: photo fingerprints and duplicate links (photos themselves are not stored).
- `bot_access`: access requests and decisions.

Evaluation statuses: `rejected_non_listing`, `rejected_basic`, `rejected_location`, `duplicate`, `rejected_ai`, `passed`, `failed`.

The worker writes one JSON object per line to `.logs/pipeline/pipeline-YYYY-MM-DD.jsonl` (every stage of every listing) and `.logs/geocoding/geocoding-YYYY-MM-DD.jsonl` (the exact address sent to Google and the result). Filter them by `listing_id`, `profile_id` or `channel_username`.

## Tests

```bash
go test ./...
```

## Project layout

- `cmd/apartment-hunter`: entrypoint.
- `internal/adapters/cli`: command wiring, the listing worker and notifications.
- `internal/adapters/telegram`: the bot, access control, settings card and collage rendering.
- `internal/adapters/telegramuserbot`: MTProto client (history fetch and live updates).
- `internal/adapters/mcpagent`: the conversational agent and its tools.
- `internal/adapters/openaipreferences`, `internal/adapters/openaivision`: preferences parser and photo analysis (Anthropic or OpenAI-compatible).
- `internal/adapters/googlemaps`: geocoding and static maps.
- `internal/listingparser`: listing text parsing.
- `internal/preferenceparser`: search-area geometry.
- `internal/evaluation`, `internal/scoring`, `internal/dedup`: pipeline, scoring and image deduplication.
- `internal/storage/postgres`: schema and persistence.
- `internal/cities`: supported cities.
- `scripts`: session string generator and test helpers.
- `housing_facts`: notes about the Batumi rental market.

`AGENTS.md` is a more detailed map of the code for contributors and coding agents.
