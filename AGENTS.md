# Agent Orientation

This is a short map for future agents. Treat it as a starting point, then verify details in code before changing behavior.

## Inspecting Telegram Posts

When you need to look at an actual Telegram post (for example, to reproduce a feedback or check what text/price a listing really contained), you MUST read it through the Telegram userbot/MCP, never through a web browser or by guessing from `t.me/...` links.

- Use the userbot client in `internal/adapters/telegramuserbot` (`Fetch`/`Listen`), which authenticates with the `TELEGRAM_*` env vars in `.env`.
- The bot's MCP mode (`/mcp`) is for managing preferences conversationally; for reading raw channel history use the userbot fetch path.
- Browser scraping of `t.me` is unreliable (albums, hidden text, auth) and is not allowed for this repo.

## Big Picture

`apartment-hunter` evaluates Batumi rental listings against a Telegram user's preferences.

- `cmd/apartment-hunter/main.go` starts the CLI.
- `internal/adapters/cli` wires commands, env vars, Postgres, Telegram, OpenAI, geocoding, and processing.
- `internal/domain` defines preferences, listings, search geometry, evaluation records, and feedback.
- `internal/storage/postgres` owns schema and persistence.
- `internal/evaluation` is the main listing processing pipeline.

## Telegram Profiles And Subscriptions

Profiles are keyed as `telegram_<chat_id>` via `profiles.IDForTelegramChat`.

Bot commands live in `internal/adapters/telegram/bot.go`:

- `/start`, `/preferences`, `/restart` start preference collection.
- A free-text query is parsed by `preferenceparser.Service` using the OpenAI preferences adapter.
- On Save, `saveDraft` saves two things:
  - latest preferences in `profiles.preferences`;
  - a historical search profile in `user_search_profiles`, including `search_area` geometry when it can be built.
- `/subscribe`, pasted `t.me/name`, and `@name` add rows to `profile_channels`.
- `/unsubscribe` removes rows from `profile_channels`.
- The bot never joins chats automatically. The Telegram userbot account must already have access.

Postgres tables to inspect first:

- `profiles`: Telegram chat id, current preferences, debug mode.
- `profile_channels`: subscriptions, username/title, cursor JSON, history limit.
- `user_search_profiles`: historical preference snapshots and latest PostGIS search area.
- `listing_evaluations`: processed messages and notification ids.

## Reading Telegram Messages

There are two userbot paths in `internal/adapters/telegramuserbot/client.go`:

- `Fetch`: reads recent channel history with `messages.getHistory`.
- `Listen`: opens a live MTProto session and handles `UpdateNewMessage` / `UpdateNewChannelMessage`.

`telegram-read` in `internal/adapters/cli/cli.go`:

- reads each subscribed channel with `Fetch`;
- uses the subscription cursor (`LastMessageID`);
- writes the updated cursor with `UpdateProfileChannelCursor`;
- prints JSON previews; it does not run the full evaluation pipeline.

`telegram-process` in `internal/adapters/cli/cli.go`:

- loads the profile, preferences, and channel subscriptions;
- builds an `evaluation.Processor`;
- first runs `catchUpTelegramHistory`, then starts `reader.Listen`.

Important current behavior: `catchUpTelegramHistory` fetches recent messages from each subscription using an empty cursor and the subscription `Limit` (default 50). It skips already-processed messages via `EvaluationExists(profileID, channelID, messageID)`. It does not advance `profile_channels.cursor`. If a message seems "stuck", check `listing_evaluations` for that profile/channel/message and check whether `channel_id` changed from username to numeric Telegram id after resolve.

Live messages are filtered by `profileSubscribedToMessage`, which matches subscriptions against both channel id and username after trimming `@` and lowercasing.

Grouped Telegram albums are coalesced:

- live updates wait `albumSettleDelay` before handling a grouped message;
- fetched history merges messages with the same channel and grouped id.

## Listing Processing Pipeline

The main flow is `evaluation.Processor.ProcessMessage`:

1. Parse `sources.ChannelMessage` into `domain.ApartmentListing`.
2. Reject obvious non-listings.
3. Apply basic filters from preferences: price, area, required amenities.
4. Geocode the listing if coordinates are missing.
5. Check location against the latest saved search area with PostGIS.
6. Download Telegram photos only after the cheap filters pass.
7. Check image deduplication.
8. Call OpenAI Vision for interior/view/amenity insights.
9. Score the listing and save `listing_evaluations`.
10. Optionally send a Telegram report and search-area map.

Statuses to know: `received`, `rejected_non_listing`, `rejected_basic`, `rejected_location`, `duplicate`, `rejected_ai`, `passed`, `failed`.

Debug mode controls notification volume:

- debug on: send reports for every processed message;
- debug off: send only `passed` listings.

## Runtime JSONL Logs

`telegram-process` writes short-lived text logs under `.logs/` by default. The directory is gitignored. Logs are JSONL: one JSON object per line, so use `tail`, `rg`, `jq`, or filter by `listing_id`, `message_id`, `profile_id`, `telegram_chat_id`, `channel_id`, or `channel_username`.

Retention defaults to 3 days. Override with:

- `APARTMENT_HUNTER_PIPELINE_LOG_DIR`
- `APARTMENT_HUNTER_PIPELINE_LOG_RETENTION_DAYS`
- `APARTMENT_HUNTER_GEOCODING_LOG_DIR`
- `APARTMENT_HUNTER_GEOCODING_LOG_RETENTION_DAYS`

Pipeline logs live at `.logs/pipeline/pipeline-YYYY-MM-DD.jsonl`.

They record processing decisions for each listing, including source context and listing summary fields: `profile_id`, `telegram_chat_id`, `channel_id`, `channel_username`, `channel_name`, `message_id`, `message_link`, `grouped_id`, `listing_id`, `listing_title`, `stage`, `status`, `reason`, `price`, `area_m2`, coordinates, `photo_count`, `hashable_photo_count`, `duration_ms`, and `error`.

Important stages include:

- `received`, `parsed`, `non_listing_filter`, `basic_filter`;
- `geocoded`, `location_checked`;
- `photos_loaded`, `dedup_checked`, `dedup_saved`;
- `vision_called`, `scored`, `saved`;
- `notification_sent`, `notification_saved`.

Geocoding logs live at `.logs/geocoding/geocoding-YYYY-MM-DD.jsonl`.

They record every listing geocoder attempt after basic filters pass, with the same source/listing context plus `query`, `max_distance_meters`, `result_name`, result coordinates, `success`, and `error`. For the Google adapter, `query` is the actual address string sent to Google after default-region expansion, which is useful for spotting Batumi/Tbilisi mixups.

## Geometry And Location Matching

Preference geometry is built in `internal/preferenceparser/geometry.go` by `PostGISGeometryBuilder`.

Input is `domain.LocationPreference` from the preference parser. For each location:

- resolve center from explicit coordinates, known Batumi centers, or Google geocoder;
- build a circle polygon from radius;
- optionally clip by boundary street, currently with Pushkin support and a fallback hardcoded line;
- optionally clip by the sea-distance corridor;
- combine polygons into a WKT/GeoJSON `MultiPolygon`.

`SaveSearchProfile` stores the WKT as `user_search_profiles.search_area` with SRID 4326.

At evaluation time, `Repository.MatchesListingLocation`:

- takes the latest non-null `search_area` for the profile;
- rejects unresolved listing coordinates with `listing location is unresolved`;
- checks `ST_Covers(search_area, ST_MakePoint(longitude, latitude))`;
- rejects outside points with `listing location is outside preferred search area`.

Search-area map rendering uses the latest GeoJSON via `LatestSearchAreaGeoJSON`.

## Practical Debug Checklist

For subscription or offset issues:

- Check `.logs/pipeline/pipeline-YYYY-MM-DD.jsonl` for `received`, skip-adjacent stages, final `saved` status, and notification stages.
- Confirm `profiles.id = telegram_<chat_id>` exists and has `preferences`.
- Inspect `profile_channels` for username, numeric `channel_id`, `cursor`, and `limit_count`.
- For `telegram-process`, inspect `listing_evaluations` rather than only `profile_channels.cursor`.
- Compare subscription username/id with incoming `ChannelMessage.ChannelID` and `ChannelUsername`.
- Remember `Fetch` resolves public username to numeric Telegram channel id.

For geo issues:

- Check `.logs/geocoding/geocoding-YYYY-MM-DD.jsonl` to see the exact query sent to Google and the returned coordinates/name.
- Check `.logs/pipeline/pipeline-YYYY-MM-DD.jsonl` for `geocoded` and `location_checked` stages.
- Inspect the latest row in `user_search_profiles` for `preferences_json`, `search_area`, and unresolved rules.
- Check whether the listing has coordinates before PostGIS matching.
- If rejected by location, compare listing point against `LatestSearchAreaGeoJSON` or `ST_Covers` manually.

For expensive pipeline issues:

- Basic and location rejections happen before photo download and OpenAI Vision.
- Local Telegram photos are downloaded under `TELEGRAM_MEDIA_DIR`, then redacted from saved records as `local-photo-redacted`.
- AI usage/cost is stored in `listing_evaluations` when Vision runs.

## Testing Changes via Telegram MCP

When modifying the preference parser, bot logic, or database storage:

- Refer to the [telegram-bot-communication skill](.agents/skills/telegram-bot-communication/SKILL.md) for detailed instructions on bot commands, modes (MCP/Debug/Regular), and how to configure/run the bot for testing.
- Agents MUST verify the changes interactively using the Telegram MCP bot interface.
- **CRITICAL:** Do NOT use browser tools or open web browsers (such as `browser_subagent` or navigating to `web.telegram.org` in a browser) for testing. Browser scraping is unreliable, and browser authentication is locked.
- **HOW TO TEST:** Write and execute a programmatic Python script in the `scratch/` directory using the `telethon` library (using the virtual environment `/Users/dmemruk/development/go/telegram-mcp-app/.venv` and account session credentials from the local `.env` file). The script must act as a test user client, sending commands/messages to the bot (e.g., `@Apartmenthuner_bot`), sleeping to wait for responses, reading the responses, and verifying the conversational flow and formatting.
- If the test script/subagent detects any errors, incorrect behavior, or crash logs, the agent must treat it as a failed validation and return the code for rework.
- Once the validation passes successfully, agents MUST immediately commit all production and configuration changes.

