package mcpagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"apartment-hunter/internal/cities"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"
)

const (
	defaultAnthropicModel = "claude-haiku-5-5"
	maxAgentToolRounds    = 8
	accountLookupTimeout  = 10 * time.Second
	emptyAgentReply       = "Не получилось довести действие до конца. Сформулируй просьбу иначе или спроси «покажи мой поиск», чтобы проверить настройки."
)

type ProfileStore interface {
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	SearchEvaluations(ctx context.Context, profileID string, filter domain.EvaluationSearchFilter) ([]domain.ListingEvaluationRecord, error)
	GetEvaluationByTelegramMessage(ctx context.Context, profileID string, telegramChatID int64, messageID int) (domain.ListingEvaluationRecord, bool, error)
}

type Geocoder interface {
	Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error)
}

type TelegramChatLister interface {
	ListAvailableChats(ctx context.Context, limit int) ([]sources.AvailableChat, error)
	AccountUserID(ctx context.Context) (int64, error)
}

type Agent struct {
	apiKey     string
	model      string
	apiURL     string
	ttlHours   int
	client     *http.Client
	store      ProfileStore
	geocoder   Geocoder
	chatLister TelegramChatLister
	anthropic  *anthropic.Client
	effort     string
	sources    SourceManager
}

func New(apiKey string, store ProfileStore) *Agent {
	apiURL := strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL"))
	if apiURL == "" {
		apiURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	}
	if apiURL == "" {
		if strings.HasPrefix(apiKey, "sk-or-") || os.Getenv("OPENROUTER_API_KEY") != "" {
			apiURL = "https://openrouter.ai/api/v1/chat/completions"
		} else {
			apiURL = "https://api.openai.com/v1/chat/completions"
		}
	} else {
		apiURL = strings.TrimRight(apiURL, "/") + "/chat/completions"
	}
	model := strings.TrimSpace(os.Getenv("OPENROUTER_PREFERENCES_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OPENAI_PREFERENCES_MODEL"))
	}
	if model == "" {
		model = "gpt-5.4-mini"
	}

	ttlHours := 24
	if envTTL := strings.TrimSpace(os.Getenv("MCP_CONTEXT_TTL_HOURS")); envTTL != "" {
		if parsed, err := strconv.Atoi(envTTL); err == nil {
			ttlHours = parsed
		}
	}

	return &Agent{
		apiKey:   apiKey,
		model:    model,
		apiURL:   apiURL,
		ttlHours: ttlHours,
		client:   &http.Client{Timeout: 60 * time.Second},
		store:    store,
	}
}

func (a *Agent) SetGeocoder(geocoder Geocoder) {
	a.geocoder = geocoder
}

func (a *Agent) SetTelegramChatLister(chatLister TelegramChatLister) {
	a.chatLister = chatLister
}

func (a *Agent) mayListTelegramChats(ctx context.Context, profile profiles.Profile) bool {
	if a.chatLister == nil || profile.TelegramChatID == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, accountLookupTimeout)
	defer cancel()
	accountID, err := a.chatLister.AccountUserID(ctx)
	return err == nil && accountID == profile.TelegramChatID
}

func NewAnthropic(apiKey string, store ProfileStore, opts ...option.RequestOption) *Agent {
	agent := New(apiKey, store)
	agent.model = strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if agent.model == "" {
		agent.model = defaultAnthropicModel
	}
	agent.effort = strings.TrimSpace(os.Getenv("ANTHROPIC_EFFORT"))
	client := anthropic.NewClient(append([]option.RequestOption{option.WithAPIKey(apiKey), option.WithRequestTimeout(90 * time.Second)}, opts...)...)
	agent.anthropic = &client
	return agent
}

func FromEnv(store ProfileStore) (*Agent, error) {
	if anthropicKey := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); anthropicKey != "" {
		return NewAnthropic(anthropicKey, store), nil
	}
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if apiKey == "" {
		return nil, fmt.Errorf("neither OPENROUTER_API_KEY nor OPENAI_API_KEY is configured for MCP agent")
	}
	return New(apiKey, store), nil
}

type chatRequest struct {
	Model      string        `json:"model"`
	Messages   []chatMessage `json:"messages"`
	Tools      []chatTool    `json:"tools,omitempty"`
	ToolChoice string        `json:"tool_choice,omitempty"`
}

type chatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`

	anthropicContent []anthropic.ContentBlockParamUnion
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (a *Agent) ProcessCommand(ctx context.Context, profileID string, text string) (string, *domain.UserPreferences, *domain.GeoSearchArea, error) {
	profile, ok, err := a.store.GetProfile(ctx, profileID)
	if err != nil {
		return "", nil, nil, err
	}
	if !ok {
		return "Профиль не найден", nil, nil, nil
	}
	state, err := a.conversationState(ctx, profile)
	if err != nil {
		return "", nil, nil, err
	}
	canListChats := a.mayListTelegramChats(ctx, profile)
	return a.converse(ctx, profile, conversationTurn{
		systemPrompt:   commandSystemPrompt(profile, a.mayManageSources(profile), canListChats, state),
		userContent:    text,
		historyContent: text,
		canListChats:   canListChats,
	})
}

func (a *Agent) ProcessFeedback(ctx context.Context, profileID string, listing domain.ApartmentListing, feedbackText string, telegramChatID int64, repliedMessageID int) (string, *domain.UserPreferences, *domain.GeoSearchArea, error) {
	profile, ok, err := a.store.GetProfile(ctx, profileID)
	if err != nil {
		return "", nil, nil, err
	}
	if !ok {
		return "Профиль не найден", nil, nil, nil
	}
	state, err := a.conversationState(ctx, profile)
	if err != nil {
		return "", nil, nil, err
	}
	listingJSON, _ := json.MarshalIndent(listing, "", "  ")
	return a.converse(ctx, profile, conversationTurn{
		systemPrompt:   feedbackSystemPrompt(a.mayManageSources(profile), state),
		userContent:    fmt.Sprintf("Контекст: Я ответил на следующее объявление.\ntelegram_chat_id: %d\nreplied_message_id: %d\n\nДанные объявления:\n%s\n\nМой отзыв: %s", telegramChatID, repliedMessageID, string(listingJSON), feedbackText),
		historyContent: "[Отзыв на объявление] " + feedbackText,
	})
}

const agentIntroduction = `Ты — ассистент Telegram-бота, который ищет квартиры в аренду: он читает Telegram-каналы и группы с объявлениями, проверяет каждое объявление по профилю поиска пользователя (бюджет, площадь, район, ремонт по фото и т.д.) и присылает подходящие. Кнопок и меню у бота нет: пользователь настраивает всё, просто переписываясь с тобой. Поэтому ты сам выясняешь, что ему нужно, и сам меняешь настройки инструментами.`

func agentAbilities(canManageSources, canListChats bool) string {
	lines := []string{
		"Что ты умеешь:",
		"- Рассказывать, какие сейчас стоят настройки и как ты видишь идеальную квартиру пользователя.",
		"- Обсуждать ремонт, район, бюджет, удобства и помогать сформулировать четкие критерии.",
		"- Менять профиль поиска инструментом update_preferences, НО только после того, как согласуешь изменения с пользователем, или если он прямо попросил их внести. Передавай только поля, которые меняются.",
		"- Управлять стоп-словами в поле stop_words.",
		"- Работать с географией: resolve_address находит координаты, calculate_distance считает расстояние, update_search_area задает зону поиска из GeoJSON.",
		"- Включать или выключать фильтрацию целиком (set_filtering_status).",
		"- Менять режим уведомлений (set_notification_mode): только подходящие объявления или отчет по каждому объявлению с причиной.",
		"- Искать по уже проверенным объявлениям (search_local_listings) и объяснять решение по конкретному объявлению (get_listing_evaluation_report).",
	}
	if canManageSources {
		lines = append(lines, "- Управлять источниками объявлений: list_sources показывает подключенные Telegram-каналы и группы, add_sources подключает новые (@username или ссылки t.me/name), remove_sources отключает. Когда пользователь присылает @username или ссылку t.me, сразу подключи ее через add_sources и перескажи результат своими словами.")
	} else {
		lines = append(lines, "- Подсказывать, как подключить источник: пользователь присылает @username или ссылку t.me/name публичного канала или группы с объявлениями.")
	}
	if canListChats {
		lines = append(lines, "- Просматривать публичные каналы и группы, доступные аккаунту-читателю (list_available_telegram_chats), чтобы предложить релевантные источники. Это НЕ список уже подключенных источников. Предлагай только чаты про аренду и недвижимость в городе поиска; если подходящих нет, так и скажи.")
	}
	lines = append(lines,
		"- Город поиска хранится в поле city (batumi — Батуми, tbilisi — Тбилиси, пусто — Батуми). Меняй city через update_preferences только когда пользователь явно просит искать в другом городе; иначе передавай текущее значение или null. При смене города районы и зона прошлого города сбрасываются: в том же вызове передай location_preferences: [] или только районы нового города, которые назвал пользователь, и не вызывай update_search_area.",
		"- Аккаунт-читатель — это аккаунт владельца бота. Объявления приходят только из чатов, в которых он состоит. Если add_sources сообщает, что он не состоит в чате, объясни, что объявления оттуда начнут приходить после того, как владелец бота вступит в этот чат.",
	)
	return strings.Join(lines, "\n")
}

const onboardingInstructions = `Сейчас у пользователя еще нет профиля поиска: ты проводишь первую настройку. Веди ее как живой разговор, а не анкету.
- Задавай один-два коротких вопроса за сообщение. Если пользователь сразу рассказал многое, не переспрашивай то, что уже известно, — переходи к недостающему.
- Выясни примерно в таком порядке: город (Батуми или Тбилиси); бюджет в месяц в USD; площадь или число комнат; районы или ориентиры и насколько далеко от них можно; что обязательно (например посудомойка, кондиционер, можно с животными); что желательно; что точно не подходит (тип ремонта, этаж, вид из окна и т.п.).
- Если пользователь хочет сам нарисовать зону на карте, сначала сохрани профиль после подтверждения сводки, а зону прими следующим сообщением.
- Для запуска поиска достаточно города, бюджета и района (или явного «район не важен»). Остальное спроси, но не настаивай, если пользователь не хочет уточнять.
- Когда данных хватает, покажи короткую сводку человеческим языком и спроси, запускать ли поиск. НЕ вызывай update_preferences, пока пользователь не подтвердил сводку («да», «ок», «запускай» и т.п.). После подтверждения сохрани все одним вызовом update_preferences.`

const configuredInstructions = `Профиль поиска пользователя уже сохранен. Помогай менять его по просьбе пользователя. Если он хочет настроить поиск заново, пройди те же вопросы, что при первой настройке (город, бюджет, площадь, районы, обязательное, желательное, неприемлемое), и сохрани результат после подтверждения сводки.
- Если источников нет, напомни, что без них искать негде, и попроси прислать каналы или группы с объявлениями.
- Если источники уже есть, а режим уведомлений с пользователем еще не обсуждали, один раз спроси, присылать только подходящие объявления или отчет по каждому, и выставь режим через set_notification_mode.
- На просьбы вроде «покажи мой поиск» дай краткую сводку: город, бюджет, площадь, районы, обязательное и неприемлемое, источники и режим уведомлений. Карту зоны поиска бот прикрепит сам.`

const conversationRules = `ВАЖНО:
- Используй Markdown разметку для оформления (списки, **жирный текст** и т.д.). Пиши коротко.
- Не упоминай технические ключи из JSON (например, location_preferences, reject_rules) и названия инструментов. Перечисляй фильтры и локации человеческим языком.
- Не предлагай пользователю команды со слэшем: всё делается обычными сообщениями. Исключение — /reset, если он хочет начать разговор с чистого листа (фильтры при этом сохраняются).
- Если в состоянии заполнен ключ "SearchArea" (даже если районы пусты), локация для поиска задана гео-зоной на карте. НИКОГДА не пиши в этом случае, что локация не задана.
- Если пользователь просит показать карту или изображение зоны поиска и "SearchArea" заполнен, коротко скажи, что прикрепляешь изображение текущей зоны. НЕ говори, что не можешь отрисовать карту: бот прикрепит ее сам.
- О подключенных источниках отвечай по "ConnectedSources" из состояния или по результату list_sources. НЕ говори, что не имеешь доступа к списку чатов.
- Если пользователь хочет сам нарисовать зону поиска, дай пошаговую инструкцию: 1) открыть geojson.io, 2) нарисовать полигон инструментом Draw a polygon, 3) скопировать JSON справа и прислать в чат текстом или файлом.
- Задавай уточняющие вопросы, если пожелания размыты (например, «хочу бабушкин вариант» → предложи отсеивать советский ремонт).`

func commandSystemPrompt(profile profiles.Profile, canManageSources, canListChats bool, state string) string {
	stage := configuredInstructions
	if profile.Preferences == nil {
		stage = onboardingInstructions
	}
	return strings.Join([]string{
		agentIntroduction,
		agentAbilities(canManageSources, canListChats),
		stage,
		conversationRules,
		"Текущее состояние настроек:\n" + state,
		"Отвечай дружелюбно и по делу.",
	}, "\n\n")
}

func feedbackSystemPrompt(canManageSources bool, state string) string {
	return strings.Join([]string{
		agentIntroduction,
		"Сейчас пользователь ответил отзывом на отчет бота об объявлении. Помоги уточнить профиль поиска на основе отзыва и данных объявления. В контексте отзыва есть telegram_chat_id и replied_message_id: по ним get_listing_evaluation_report вернет полный отчет. Город в ответ на отзыв не меняй.",
		agentAbilities(canManageSources, false),
		conversationRules,
		"Текущее состояние настроек:\n" + state,
		"Отвечай дружелюбно и по делу.",
	}, "\n\n")
}

func profileSearchCity(profile profiles.Profile) cities.City {
	if profile.Preferences == nil {
		return cities.Batumi
	}
	return cities.ForKey(profile.Preferences.City, cities.Batumi)
}

func mergePreferenceUpdate(existing *domain.UserPreferences, updateJSON string) (domain.UserPreferences, error) {
	var patch domain.UserPreferences
	if err := json.Unmarshal([]byte(updateJSON), &patch); err != nil {
		return domain.UserPreferences{}, err
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(updateJSON), &fields); err != nil {
		return domain.UserPreferences{}, err
	}

	merged := domain.UserPreferences{}
	if existing != nil {
		merged = preferenceparser.NormalizePreferences(*existing)
	}

	if hasField(fields, "budget_min") {
		merged.BudgetMin = patch.BudgetMin
		if patch.BudgetMin != nil {
			merged.MinBudget = *patch.BudgetMin
		} else {
			merged.MinBudget = 0
		}
	}
	if hasField(fields, "min_budget") {
		merged.MinBudget = patch.MinBudget
		if patch.MinBudget > 0 {
			value := patch.MinBudget
			merged.BudgetMin = &value
		} else {
			merged.BudgetMin = nil
		}
	}
	if hasField(fields, "budget_max") {
		merged.BudgetMax = patch.BudgetMax
		if patch.BudgetMax != nil {
			merged.MaxBudget = *patch.BudgetMax
		} else {
			merged.MaxBudget = 0
		}
	}
	if hasField(fields, "max_budget") {
		merged.MaxBudget = patch.MaxBudget
		if patch.MaxBudget > 0 {
			value := patch.MaxBudget
			merged.BudgetMax = &value
		} else {
			merged.BudgetMax = nil
		}
	}
	if hasField(fields, "area_min") {
		merged.AreaMin = patch.AreaMin
		if patch.AreaMin != nil {
			merged.MinAreaM2 = *patch.AreaMin
		} else {
			merged.MinAreaM2 = 0
		}
	}
	if hasField(fields, "min_area_m2") {
		merged.MinAreaM2 = patch.MinAreaM2
		if patch.MinAreaM2 > 0 {
			value := patch.MinAreaM2
			merged.AreaMin = &value
		} else {
			merged.AreaMin = nil
		}
	}
	if hasField(fields, "area_max") {
		merged.AreaMax = patch.AreaMax
	}
	if hasField(fields, "required_amenities") {
		merged.RequiredAmenities = patch.RequiredAmenities
		merged.RequireDishwasher = false
		merged.RequireOven = false
	}
	if hasField(fields, "require_dishwasher") {
		merged.RequireDishwasher = patch.RequireDishwasher
	}
	if hasField(fields, "require_oven") {
		merged.RequireOven = patch.RequireOven
	}
	if hasField(fields, "preferred_amenities") {
		merged.PreferredAmenities = patch.PreferredAmenities
	}
	if hasField(fields, "interior_preferences") {
		merged.InteriorPreferences = patch.InteriorPreferences
	}
	if hasField(fields, "reject") {
		merged.Reject = patch.Reject
	}
	if hasField(fields, "reject_rules") {
		merged.RejectRules = patch.RejectRules
	}
	if hasField(fields, "location_preferences") {
		merged.LocationPreferences = patch.LocationPreferences
		merged.Locations = patch.LocationPreferences
	}
	if hasField(fields, "locations") {
		merged.Locations = patch.Locations
		merged.LocationPreferences = patch.Locations
	}
	if hasField(fields, "weights") {
		merged.Weights = patch.Weights
	}
	if hasField(fields, "importance") {
		merged.Importance = patch.Importance
	}
	if hasField(fields, "unresolved_location_rules") {
		merged.UnresolvedLocationRules = patch.UnresolvedLocationRules
	}
	if hasField(fields, "has_search_area") {
		merged.HasSearchArea = patch.HasSearchArea
	}
	if hasField(fields, "stop_words") {
		merged.StopWords = patch.StopWords
	}
	if city, ok := cities.Find(patch.City); ok {
		merged.City = city.Key
	}

	return preferenceparser.NormalizePreferences(merged), nil
}

func hasField(fields map[string]json.RawMessage, name string) bool {
	_, ok := fields[name]
	return ok
}

func haversineDistance(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000.0 // meters
	dLat := (lat2 - lat1) * math.Pi / 180.0
	dLon := (lon2 - lon1) * math.Pi / 180.0
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*math.Pi/180.0)*math.Cos(lat2*math.Pi/180.0)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadius * c
}

type geoJSONGeometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates,omitempty"`
}

type geoJSONFeature struct {
	Type     string          `json:"type"`
	Geometry json.RawMessage `json:"geometry"`
}

type geoJSONFeatureCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
}

func parseGeoJSONArea(text string) (*domain.GeoSearchArea, error) {
	polygons, err := geoJSONMultiPolygonCoordinates([]byte(strings.TrimSpace(text)))
	if err != nil {
		return nil, err
	}
	if len(polygons) == 0 {
		return nil, fmt.Errorf("GeoJSON does not contain Polygon or MultiPolygon")
	}
	normalized, err := json.Marshal(map[string]any{
		"type":        "MultiPolygon",
		"coordinates": polygons,
	})
	if err != nil {
		return nil, err
	}
	return &domain.GeoSearchArea{
		WKT:     multiPolygonCoordinatesWKT(polygons),
		GeoJSON: string(normalized),
		SRID:    4326,
	}, nil
}

func geoJSONMultiPolygonCoordinates(data []byte) ([][][][]float64, error) {
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return nil, fmt.Errorf("failed to parse GeoJSON type: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(typed.Type)) {
	case "featurecollection":
		var collection geoJSONFeatureCollection
		if err := json.Unmarshal(data, &collection); err != nil {
			return nil, fmt.Errorf("failed to parse FeatureCollection: %w", err)
		}
		var polygons [][][][]float64
		for _, feature := range collection.Features {
			if len(feature.Geometry) == 0 || string(feature.Geometry) == "null" {
				continue
			}
			featurePolygons, err := geoJSONMultiPolygonCoordinates(feature.Geometry)
			if err != nil {
				return nil, err
			}
			polygons = append(polygons, featurePolygons...)
		}
		return validateMultiPolygonCoordinates(polygons)
	case "feature":
		var feature geoJSONFeature
		if err := json.Unmarshal(data, &feature); err != nil {
			return nil, fmt.Errorf("failed to parse Feature: %w", err)
		}
		if len(feature.Geometry) == 0 || string(feature.Geometry) == "null" {
			return nil, fmt.Errorf("Feature is missing geometry")
		}
		return geoJSONMultiPolygonCoordinates(feature.Geometry)
	case "polygon":
		var geometry geoJSONGeometry
		if err := json.Unmarshal(data, &geometry); err != nil {
			return nil, fmt.Errorf("failed to parse Polygon: %w", err)
		}
		var polygon [][][]float64
		if err := json.Unmarshal(geometry.Coordinates, &polygon); err != nil {
			return nil, fmt.Errorf("failed to parse Polygon coordinates: %w", err)
		}
		return validateMultiPolygonCoordinates([][][][]float64{polygon})
	case "multipolygon":
		var geometry geoJSONGeometry
		if err := json.Unmarshal(data, &geometry); err != nil {
			return nil, fmt.Errorf("failed to parse MultiPolygon: %w", err)
		}
		var polygons [][][][]float64
		if err := json.Unmarshal(geometry.Coordinates, &polygons); err != nil {
			return nil, fmt.Errorf("failed to parse MultiPolygon coordinates: %w", err)
		}
		return validateMultiPolygonCoordinates(polygons)
	default:
		return nil, fmt.Errorf("only GeoJSON Polygon, MultiPolygon, Feature, and FeatureCollection are supported")
	}
}

func validateMultiPolygonCoordinates(polygons [][][][]float64) ([][][][]float64, error) {
	for i := range polygons {
		if len(polygons[i]) == 0 {
			return nil, fmt.Errorf("polygon %d has no rings", i+1)
		}
		for j := range polygons[i] {
			ring := polygons[i][j]
			if len(ring) < 4 {
				return nil, fmt.Errorf("polygon %d ring %d has fewer than 4 points", i+1, j+1)
			}
			for k, coord := range ring {
				if len(coord) < 2 {
					return nil, fmt.Errorf("polygon %d ring %d point %d has fewer than 2 coordinates", i+1, j+1, k+1)
				}
				lng, lat := coord[0], coord[1]
				if math.IsNaN(lng) || math.IsInf(lng, 0) || math.IsNaN(lat) || math.IsInf(lat, 0) {
					return nil, fmt.Errorf("polygon %d ring %d point %d has non-numeric coordinate", i+1, j+1, k+1)
				}
				if lng < -180 || lng > 180 || lat < -90 || lat > 90 {
					return nil, fmt.Errorf("polygon %d ring %d point %d is out of range", i+1, j+1, k+1)
				}
				polygons[i][j][k] = []float64{lng, lat}
			}
			if len(ring[0]) >= 2 && len(ring[len(ring)-1]) >= 2 && (ring[0][0] != ring[len(ring)-1][0] || ring[0][1] != ring[len(ring)-1][1]) {
				polygons[i][j] = append(polygons[i][j], []float64{ring[0][0], ring[0][1]})
			}
		}
	}
	return polygons, nil
}

func multiPolygonCoordinatesWKT(polygons [][][][]float64) string {
	var b strings.Builder
	b.WriteString("MULTIPOLYGON(")
	for i, polygon := range polygons {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(")
		for j, ring := range polygon {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString("(")
			for k, coord := range ring {
				if k > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, "%.8f %.8f", coord[0], coord[1])
			}
			b.WriteString(")")
		}
		b.WriteString(")")
	}
	b.WriteString(")")
	return b.String()
}

func (a *Agent) makeChatRequest(ctx context.Context, reqBody chatRequest) (chatMessage, error) {
	if a.anthropic != nil {
		return a.makeAnthropicRequest(ctx, reqBody)
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return chatMessage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL, bytes.NewReader(body))
	if err != nil {
		return chatMessage{}, err
	}

	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return chatMessage{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return chatMessage{}, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return chatMessage{}, fmt.Errorf("openai chat api status %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return chatMessage{}, err
	}

	if len(result.Choices) == 0 {
		return chatMessage{}, fmt.Errorf("empty choices in response")
	}

	return result.Choices[0].Message, nil
}
