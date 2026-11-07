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
- The bot never joins chats automatically. The Telegram userbot account must already be a member: live updates only arrive from joined chats. On subscribe the bot resolves the username through the userbot (`ChannelAccess`), refuses unknown usernames and warns when the userbot is not a member.
- In MCP mode an active `/preferences` session still takes priority, so the preview and Save flow work; `/mcp` ends any open session. The MCP agent may chain up to `maxAgentToolRounds` tool rounds per message, and `list_available_telegram_chats` is offered only to the profile whose chat id equals the userbot account id.

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

`telegram-process [profile_id]` and the worker goroutine of the `telegram` command serve every profile that has `profile_channels` rows (or only `profile_id` when given). The logic lives in `internal/adapters/cli/listing_worker.go`:

- one `reader.Listen` session; `listingWorker.HandleLiveMessage` loads the subscribed profiles for each message and runs the pipeline separately for every profile subscribed to that channel;
- `CatchUpNewSubscriptions` runs at start and then every minute; it fetches recent history (empty cursor, subscription `Limit`, default 50) once per new profile/channel pair, and only after the profile has preferences; a failing pair (private channel, wrong username) is retried with exponential backoff up to 6 hours to protect the shared userbot session from flood limits;
- processing is serialized and skips messages already in `listing_evaluations` via `EvaluationExists(profileID, channelID, messageID)`;
- `profile_channels.cursor` is not advanced.

If a message seems "stuck", check `listing_evaluations` for that profile/channel/message and check whether `channel_id` changed from username to numeric Telegram id after resolve.

Evaluation `listing_id` values are profile-scoped (`<profile_id>/<channel>:<message>`, see `evaluation.ProfileScopedListingID`), so two profiles on one channel keep separate records; rows saved before 2026-11 are unscoped. Image dedup only compares fingerprints of the same profile (`dedup.ProfileScopedRepository` → `ListImageFingerprintsForProfile`, which also includes legacy unscoped images whose evaluation belongs to that profile).

## Search Cities

Each profile searches in one city stored as `city` (`batumi` or `tbilisi`) inside its preferences JSON; see `internal/cities`. An empty value means Batumi. The bot takes the city from the query text, otherwise keeps the profile's current city; `/city tbilisi` switches it and clears the previous city's districts and search area, so the user sets districts again via `/preferences`. In MCP mode the agent changes the city only through an explicit `city` value in `update_preferences` (null keeps it; `resolve_address` takes an optional `city`); the bot then clears the old city's districts and area unless the same patch brings new location preferences, which are rebuilt for the new city. Listing feedback never changes the city. The city drives listing address geocoding (region suffix and max distance from the city center), search-area geometry, the geojson.io link and MCP `resolve_address`. Batumi-only shortcuts (Pushkin boundary, New Boulevard, 6 May park, 1 km sea corridor) apply only to Batumi profiles. A stored search area is inherited on save only within the same city and only when the save carries no area at all (an MCP or feedback edit that leaves the location preferences unchanged); a built area with no polygons, for example after removing all districts, clears it. A district without a radius gets a `DefaultDistrictRadiusMeters` (1500 m) circle around its geocoded center.

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
- `APARTMENT_HUNTER_LOG_TO_STDOUT=true` duplicates pipeline/geocoding JSONL events to stdout so Coolify container logs can show detailed processing decisions.

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

## Production And Coolify

Production is deployed in Coolify as project `Apartment-hunter bot`, application `apartment-hunter`, with standalone Postgres database `apartment-hunter-db`.

Use the Coolify MCP endpoint `https://coolify.memruk.dev/mcp` with a Bearer API token supplied by the user or environment; never commit the token. The current Coolify MCP server exposes inventory tools such as `get_infrastructure_overview`, `list_applications`, `get_application`, `list_databases`, and `get_database`, but it does not expose a log-reading tool. Agents can use MCP to confirm app/database status, but cannot rely on it alone to fetch container stdout/stderr or JSONL files unless the MCP server gains log tools later.

Coolify container logs show the process stdout/stderr. The detailed pipeline/geocoding JSONL logs are file logs controlled by `APARTMENT_HUNTER_PIPELINE_LOG_DIR` and `APARTMENT_HUNTER_GEOCODING_LOG_DIR`; set `APARTMENT_HUNTER_LOG_TO_STDOUT=true` to duplicate those JSONL events into Coolify logs. In `docker-compose.yaml`, only `/app/.telegram` is mounted as the `telegram-session` volume; default `/app/.logs/...` files are therefore container-local and may disappear on redeploy. For persistent production JSONL logs, set:

- `APARTMENT_HUNTER_PIPELINE_LOG_DIR=/app/.telegram/logs/pipeline`
- `APARTMENT_HUNTER_GEOCODING_LOG_DIR=/app/.telegram/logs/geocoding`
- `APARTMENT_HUNTER_LOG_TO_STDOUT=true`

If a future agent must debug production via Coolify, first check Coolify app/database status via MCP, then use Coolify UI/API/SSH or an added MCP log tool for container logs. For deep listing decisions, inspect the persisted JSONL log directory above and Postgres `listing_evaluations`.

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
- Inspect `listing_evaluations` rather than only `profile_channels.cursor`.
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

Когда агент вносит любые изменения, связанные с ботом (включая логику команд, парсер предпочтений, работу с базой данных или формат отчетов/сообщений, отправляемых ботом), он **ОБЯЗАН** протестировать эти изменения интеграционными тестами с помощью Telegram MCP.

- См. [telegram-bot-communication skill](.agents/skills/telegram-bot-communication/SKILL.md) для получения подробных инструкций о командах бота, режимах работы и конфигурировании.
- Агенты **ОБЯЗАНЫ** проверять изменения интерактивно с использованием Telegram MCP-интерфейса бота.
- **КРИТИЧЕСКИ ВАЖНО:** Не используйте браузерные инструменты или открытие браузера для тестов.
- **КАК ТЕСТИРОВАТЬ:** Используйте глобальный MCP-сервер `https://tg-mcp.memruk.dev/mcp`. Для этого вы можете использовать любые MCP-совместимые инструменты или общаться с ботом напрямую в Telegram. Убедитесь, что ваш MCP-клиент настроен на этот URL и использует правильный токен авторизации. В локальной среде предпочитайте передавать токен через `TELEGRAM_MCP_AUTH_TOKEN`; не сохраняйте токен в файлах репозитория.
- Если скрипт тестирования обнаружит ошибки, неверное поведение или логи падения, агент должен считать валидацию проваленной и вернуть код на доработку.
- После успешного прохождения валидации агент **ОБЯЗАН** немедленно закоммитить все рабочие и конфигурационные файлы.

## Housing Facts Knowledge Base

The repository contains a housing facts knowledge base in `housing_facts/` organized as an LLM Wiki (raw inputs vs compiled wiki pages).

When users provide summaries, articles, or new facts, agents must:
- Store the raw source text in `housing_facts/raw/<slug>.md`.
- Extract and compile the insights to `housing_facts/wiki/general.md` and/or building-specific pages under `housing_facts/wiki/buildings/`.
- Cross-reference compiled facts back to the raw source file using markdown links: `[[Source](file:///Users/dmemruk/development/services/apartment-hunter/housing_facts/raw/<file>.md#L<line>)]`.
- **Token Optimization:** Avoid token-heavy ASCII or Unicode diagrams, directory trees with characters like `│` or `├──`, and horizontal lines. Use simple, compact markdown lists. Keep content highly dense and direct.
