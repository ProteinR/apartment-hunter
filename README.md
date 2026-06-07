# apartment-hunter

MVP сервиса для оценки квартир в аренду в Батуми по персональным критериям.

Проект разделяет интервью пользователя, доменную модель и оценку объявления. Telegram или другой канал можно добавить позже отдельным adapter без изменения core scoring logic.

## Структура

- `cmd/apartment-hunter` - CLI entrypoint.
- `internal/domain` - доменные сущности.
- `internal/interview` - сценарий первичного опроса.
- `internal/preferences` - сохранение и чтение `preferences.yaml`.
- `internal/scoring` - hard filters, scoring и итоговый JSON.
- `internal/dedup` - image fingerprinting и listing-level дедупликация.
- `internal/adapters/cli` - CLI adapter.
- `internal/adapters/telegram` - Telegram adapter через `github.com/go-telegram/bot` long polling.
- `internal/adapters/telegramuserbot` - Telegram Userbot/MTProto adapter через `github.com/gotd/td` для чтения каналов.
- `internal/profiles` - per-user profiles: Telegram chat id, preferences, channel subscriptions and cursors.
- `internal/sources` - общий контракт источника сообщений каналов.
- `internal/adapters/visionmock` - mock evaluator фотографий и описания.
- `internal/adapters/openaivision` - real evaluator через OpenAI Responses API с Vision input и structured JSON output.
- `internal/adapters/geocodingmock` - mock координат районов.
- `internal/evaluation` - end-to-end обработка объявления: базовые фильтры, dedup, AI анализ, scoring, запись истории.
- `internal/storage/postgres` - Postgres repository для профилей, preferences, истории проверок, image hashes и duplicate records.
- `configs` - пример предпочтений.
- `examples` - sample listings.

## Запуск

Требуется Go 1.25+.

Локальные секреты храните в `.env`:

```bash
cp .env.example .env
set -a
source .env
set +a
```

```bash
go run ./cmd/apartment-hunter interview preferences.yaml
```

Команда задаст вопросы и сохранит `preferences.yaml`.

Оценить пример объявления:

```bash
go run ./cmd/apartment-hunter score examples/sample_listing.json configs/default_preferences.yaml
```

По умолчанию `score` использует mock evaluator. Для реального Vision анализа через OpenAI API:

```bash
OPENAI_API_KEY=sk-... APARTMENT_HUNTER_VISION=openai \
  go run ./cmd/apartment-hunter score listing.json configs/default_preferences.yaml
```

Опционально можно задать модель:

```bash
OPENAI_VISION_MODEL=gpt-4.1-mini
```

OpenAI adapter берет текст объявления (`title`, `description`, metadata) и фотографии из `photos[].url`, скачивает `http(s)` URL или читает локальный путь / `file://` URL, уменьшает изображения до max side `768px`, перекодирует в JPEG data URL и отправляет их в Responses API. Ответ запрашивается как structured JSON по интерфейсу `ListingInsights`:

- `style`
- `has_glossy_ceilings`
- `has_dark_walls`
- `has_old_renovation`
- `windows_facing_house`
- `view_quality`
- `interior_quality`
- `description_highlights`

Запустить Telegram adapter:

```bash
DATABASE_URL='postgres://apartment_hunter:apartment_hunter@localhost:5432/apartment_hunter?sslmode=disable' \
TELEGRAM_BOT_TOKEN=123:abc \
  go run ./cmd/apartment-hunter telegram
```

В боте отправьте `/start`. Adapter использует тот же пошаговый `internal/interview` engine, что и CLI. После завершения опроса он сохраняет профиль `telegram_<chat_id>` и preferences в Postgres.

Локации можно указывать словами или координатами. Если задан `APARTMENT_HUNTER_GOOGLE_MAPS_API_KEY`, бот геокодит названия через Google Maps Geocoding API. По умолчанию короткие названия ищутся с регионом `Batumi, Georgia`, чтобы `Europe Square` не резолвился в другой город. Несколько кастомных мест разделяйте `;`, потому что запятая используется в координатах и адресах:

```text
Europe Square
41.6466, 41.6266
Europe Square; Old Boulevard; 41.6466, 41.6266
```

Дистанция до квартиры считается по координатам через haversine. Worker сначала пытается взять координаты из текста объявления или Google Maps ссылки, например `?q=41.6466,41.6266`; если координат нет и задан Google Maps API key, он пробует геокодить текст объявления.

Postgres-профили позволяют одному боту обслуживать разных людей без хардкода критериев. Каждый профиль привязан к `telegram_<chat_id>` и хранит preferences и список каналов.

Каналами также можно управлять прямо из Telegram-бота:

```text
/channels
/subscribe https://t.me/example_channel Batumi rentals
/subscribe @example_channel Batumi rentals
/unsubscribe @example_channel
```

Бот не вступает в чаты и группы автоматически. Сначала вступите в подходящие
Telegram-источники вручную с аккаунта, которым запускается worker, затем
пришлите боту public ссылки `t.me/name` или usernames `@name`. Если источники
уже добавлены в профиль, `/channels` покажет текущий список.

Вручную прочитать историю каналов через Telegram Userbot API:

```bash
TELEGRAM_API_ID=12345 \
TELEGRAM_API_HASH=abc123 \
TELEGRAM_PHONE=+995555000000 \
TELEGRAM_SESSION_FILE=.telegram/userbot.session \
TELEGRAM_MEDIA_DIR=.telegram/media \
DATABASE_URL='postgres://apartment_hunter:apartment_hunter@localhost:5432/apartment_hunter?sslmode=disable' \
  go run ./cmd/apartment-hunter telegram-read telegram_42
```

При первом запуске `gotd/td` запросит одноразовый Telegram code в терминале и сохранит user session в `TELEGRAM_SESSION_FILE`. Если на аккаунте включен 2FA password, задайте `TELEGRAM_PASSWORD`. Команда не скачивает фото, а выводит JSON records с текстом, ссылкой, download-token для фото и распарсенным `ApartmentListing`.

Слушать только новые сообщения, сохранять историю проверки в Postgres и отправлять подходящие объявления в Telegram:

```bash
DATABASE_URL='postgres://apartment_hunter:apartment_hunter@localhost:5432/apartment_hunter?sslmode=disable' \
TELEGRAM_BOT_TOKEN=123:abc \
TELEGRAM_API_ID=12345 \
TELEGRAM_API_HASH=abc123 \
TELEGRAM_PHONE=+995555000000 \
TELEGRAM_SESSION_FILE=.telegram/userbot.session \
TELEGRAM_MEDIA_DIR=.telegram/media \
OPENAI_API_KEY=sk-... \
  go run ./cmd/apartment-hunter telegram-process telegram_42
```

`telegram-process` не читает историю канала. Он держит Telegram userbot session открытой, получает `UpdateNewMessage` / `UpdateNewChannelMessage`, фильтрует события по подпискам профиля и анализирует только новые сообщения. В stdout должны появиться строки `Listening for new Telegram messages`, `Telegram notifications enabled/disabled`, `Processed ...` и `Sending notification ...` - по ним видно, что worker жив и что уведомление ушло в бот.

Базовые отказы по цене, метражу и обязательной технике сохраняются в историю без скачивания фото и без вызова OpenAI. Если базовые фильтры прошли, userbot скачивает фото во временный local media cache, processor проверяет dedup, вызывает OpenAI Vision, считает итоговый score и сохраняет `ai_usage` с токенами и стоимостью. Если задан `TELEGRAM_BOT_TOKEN` и у профиля есть `telegram_chat_id`, worker отправляет в бот отчет по каждому обработанному сообщению из подписанных каналов, а не только по объявлениям со статусом `passed`.

OpenAI Responses API возвращает `usage` с token counts, а OpenAI pricing page публикует цены за 1M tokens. По умолчанию для `gpt-4.1-mini` используются значения:

```bash
OPENAI_VISION_INPUT_PRICE_PER_MILLION_USD=0.40
OPENAI_VISION_CACHED_INPUT_PRICE_PER_MILLION_USD=0.10
OPENAI_VISION_OUTPUT_PRICE_PER_MILLION_USD=1.60
```

Если меняете модель, обновите эти env значения по актуальной странице [OpenAI API pricing](https://platform.openai.com/docs/pricing/). Поля сохраняются в Postgres отдельно, чтобы в истории было видно количество токенов, цену за миллион токенов и рассчитанную стоимость конкретного объявления в USD.

## Docker

Docker Compose автоматически читает `.env` из директории проекта. Создайте файл и заполните значения:

```bash
cp .env.example .env
```

```bash
docker compose up --build
```

Эта команда запускает Postgres и Telegram preferences bot. Если в `.env`
задан `APARTMENT_HUNTER_PROFILE_ID` и заполнены `TELEGRAM_API_ID`,
`TELEGRAM_API_HASH`, а также `TELEGRAM_PHONE` или `TELEGRAM_SESSION_STRING`,
бот автоматически стартует worker в том же процессе. Если профиль, preferences
или каналы еще не готовы, worker будет ждать и ретраиться, а бот продолжит
работать. Сначала откройте бота в Telegram, отправьте `/start`, пройдите
интервью и добавьте каналы. Автоматического вступления нет: сначала вступите в
подходящие чаты вручную, затем пришлите public ссылки или usernames:

```text
https://t.me/example_channel
@another_rent_channel
```

После этого укажите созданный profile id в `.env` и перезапустите compose, если
профиль отличается от текущего значения:

```bash
APARTMENT_HUNTER_PROFILE_ID=telegram_42
docker compose up --build
```

Минимальные env переменные:

```bash
TELEGRAM_BOT_TOKEN=123:abc
TELEGRAM_API_ID=12345
TELEGRAM_API_HASH=abc123
TELEGRAM_PHONE=+995555000000
TELEGRAM_SESSION_STRING=
OPENAI_API_KEY=sk-...
APARTMENT_HUNTER_PROFILE_ID=telegram_42
APARTMENT_HUNTER_GOOGLE_MAPS_API_KEY=...
APARTMENT_HUNTER_GEOCODING_REGION=Batumi, Georgia
```

Postgres доступен в compose как отдельный сервис `postgres`, данные хранятся в volume `postgres-data`. Profiles, preferences и channel subscriptions хранятся в Postgres. Telegram userbot session и временно скачанные media files живут в volume `telegram-session`.

Для первого userbot login удобнее запустить worker интерактивно, чтобы ввести одноразовый Telegram code:

```bash
docker compose run --rm worker telegram-process telegram_42
```

Если уже есть Telethon session string, например из `telegram-mcp-app/session_string_generator.py`, можно задать его в `.env`:

```bash
TELEGRAM_SESSION_STRING=1...
```

`TELEGRAM_SESSION_STRING` имеет приоритет над `TELEGRAM_SESSION_FILE`, поэтому интерактивный login и volume `telegram-session` для userbot session в этом режиме не нужны. Строка должна быть создана с теми же `TELEGRAM_API_ID` и `TELEGRAM_API_HASH`, которые указаны для `apartment-hunter`.

При использовании `TELEGRAM_SESSION_STRING` переменные `TELEGRAM_PHONE` и `TELEGRAM_PASSWORD` можно оставить пустыми. Они нужны только для интерактивного логина с кодом Telegram, когда session string не задан.

Сгенерировать session string можно прямо из этого репозитория:

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -r scripts/requirements-session.txt
python scripts/session_string_generator.py --qr
```

## Postgres history

Repository создает таблицы при старте:

- `listing_evaluations` - каждое проверенное объявление, исходный listing JSON, source/message link, report, score, insights, duplicate info, status, token usage и `cost_usd`;
- `listing_images` - fingerprints/hash фотографий без хранения самих фото;
- `listing_duplicates` - связи duplicate listing -> canonical listing.

Фото не сохраняются в Postgres. Telegram-фото скачиваются только после прохождения базовых текстовых фильтров и только как локальные рабочие файлы в `TELEGRAM_MEDIA_DIR`, чтобы hasher и OpenAI Vision могли прочитать bytes. После обработки worker удаляет локальные `file://` фото. Для будущего S3-compatible хранилища достаточно добавить storage adapter и записывать object keys рядом с `listing_images`.

Собрать бинарь:

```bash
go build -o apartment-hunter ./cmd/apartment-hunter
./apartment-hunter score examples/sample_listing.json configs/default_preferences.yaml
```

## Формат результата

`score` возвращает JSON:

- `passed_hard_filters`
- `rejection_reasons`
- `score_total`
- `score_breakdown`
- `short_summary`

`telegram-process` дополнительно сохраняет и отправляет человекочитаемый отчет по каждому объявлению. Пример:

```text
Отчет по объявлению
Apartment near 6 May Park
Статус: прошло
Итог: 84/100
Сообщение: https://t.me/example_channel/123

Текст объявления:
Apartment near 6 May Park
500 USD
55 m2

Критерии:
- ✓ Метраж (55.0 м2 при минимуме 45.0)
- ✓ Цена (500, цель ближе к 400, максимум 600)
- ✓ Локация (6 May Park)
- ✓ Техника (обязательная техника есть)
- ✓ Ремонт (modern)
- ✓ Вид из окна (6/10)

Баллы:
- Расположение: 10/10 (6 May Park)
- Метраж: 10/10 (55.0 м2 при минимуме 45.0)
- Ремонт: 9/10
- Вид из окна: 6/10
- Цена: 8/10 (500, цель ближе к 400, максимум 600)
- Техника: 10/10 (обязательная техника есть)

OpenAI: gpt-4.1-mini, tokens 1842, cost $0.001215
```

Для объявлений, которые не прошли базовый этап, отчет все равно сохраняется и отправляется в бот. В чеклисте будут `✓`, `✗` и `-`: прочерк означает, что требование не задано или этап не запускался, например ремонт и вид не анализируются после отказа по цене/метражу.

Для цены можно задать не только максимальный бюджет, но и нижнюю целевую границу `min_budget`. Если указан диапазон, например `400-600`, `400` считается идеальной ценой и получает `10/10`, цена ближе к `600` получает меньше баллов, а цена выше `max_budget` отсекается.

## Тесты

```bash
go test ./...
```

## Dedup Pipeline

Перед Vision/LLM анализом новое объявление можно пропустить через `internal/dedup`:

1. Скачать или получить bytes фотографий.
2. Посчитать fingerprints: raw `sha256`, perceptual full image hash, perceptual center crop 80%, perceptual center crop 60%, full-image dHash.
3. Сравнить с сохраненными `listing_images` через `ListingDeduplicator`.
4. Если найден duplicate/probable duplicate, сохранить запись в `listing_duplicates` и не вызывать Vision evaluator.
5. Если дубля нет, сохранить fingerprints как canonical listing и продолжить Vision/scoring pipeline.

Текущие thresholds:

- exact `sha256` match - strong match;
- perceptual distance `<= 6` - strong match;
- perceptual distance `<= 10` - weak match;
- crop/full cross-match distance `<= 8` - weak match, `<= 6` - strong match.

Listing-level правила:

- `3+` matched images - duplicate, confidence `0.95`;
- `2` strong matched images - duplicate, confidence `0.90`;
- `1` strong + `2` weak matches - probable duplicate, confidence `0.85`;
- только `1` matched image - не auto-deduplicate.

Hasher реализован на стандартной библиотеке Go: average-hash используется как легкий perceptual hash, dHash считается отдельно. Это позволяет не тащить сетевую зависимость в MVP. Если понадобится более строгий pHash, adapter можно заменить на библиотеку вроде `github.com/corona10/goimagehash`, не меняя `ListingDeduplicator` и repository interfaces.

## Vision adapter

`internal/scoring` зависит от интерфейса `InsightsEvaluator`. Его реализуют `visionmock` и `openaivision`; домен и scoring logic не зависят от конкретного provider.
