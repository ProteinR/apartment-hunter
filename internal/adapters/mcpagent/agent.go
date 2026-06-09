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

	"apartment-hunter/internal/adapters/openaipreferences"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
)

type ProfileStore interface {
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	SearchEvaluations(ctx context.Context, profileID string, filter domain.EvaluationSearchFilter) ([]domain.ListingEvaluationRecord, error)
}

type Geocoder interface {
	Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error)
}

type Agent struct {
	apiKey   string
	model    string
	apiURL   string
	ttlHours int
	client   *http.Client
	store    ProfileStore
	geocoder Geocoder
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

func FromEnv(store ProfileStore) (*Agent, error) {
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
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
}

type chatMessage struct {
	Role       string    `json:"role"`
	Content    string    `json:"content"`
	Name       string    `json:"name,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
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

	var searchAreaJSON any
	if saStore, ok := a.store.(interface {
		LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error)
	}); ok {
		if geoJSON, ok, err := saStore.LatestSearchAreaGeoJSON(ctx, profileID); err == nil && ok {
			var parsed any
			if err := json.Unmarshal([]byte(geoJSON), &parsed); err == nil {
				searchAreaJSON = parsed
			} else {
				searchAreaJSON = geoJSON
			}
		}
	}

	state, err := json.MarshalIndent(map[string]any{
		"FilterDisabled": profiles.FilterDisabled(profile),
		"Preferences":    profile.Preferences,
		"SearchArea":     searchAreaJSON,
	}, "", "  ")
	if err != nil {
		return "", nil, nil, err
	}

	systemPrompt := fmt.Sprintf(`Ты - персональный ассистент по поиску квартир (MCP Agent). Твоя цель — помочь пользователю сформировать идеальные фильтры для поиска жилья.
Ты можешь:
1. Рассказывать пользователю, какие у него сейчас стоят настройки и как ты видишь его идеальную квартиру.
2. Обсуждать желаемый ремонт, район, бюджет, удобства и помогать сформулировать четкие критерии.
3. Изменять фильтры с помощью инструмента update_preferences, НО только после того, как согласуешь изменения с пользователем, или если пользователь прямо попросил обновить фильтр.
4. Включать/выключать фильтрацию с помощью set_filtering_status.
5. Работать с географией: находить координаты по адресу (resolve_address), вычислять расстояние в метрах (calculate_distance) и обновлять зону поиска напрямую с помощью GeoJSON (update_search_area).

Веди диалог живо, эмпатично и профессионально. Задавай уточняющие вопросы, если пожелания размыты (например, "хочу бабушкин вариант" -> предложи добавить "soviet" в reject_rules).

ВАЖНО:
- Используй Markdown разметку для оформления (списки, **жирный текст** и т.д.).
- Не упоминай технические ключи из JSON (например, location_preferences, reject_rules). Если выводишь список фильтров или локаций, перечисляй их естественным, человекочитаемым языком.
- Если в состоянии настроек заполнен ключ "SearchArea" (даже если текстовые предпочтения по районам пусты), это ОЗНАЧАЕТ, что локация для поиска задана (пользователь ограничил поиск гео-зоной на карте). В этом случае НИКОГДА не пиши, что локация не задана или что предпочтений по районам нет. Указывай, что поиск идет в границах заданной области/зоны поиска на карте.
- Если пользователь хочет сам нарисовать зону поиска, обязательно дай ему пошаговую инструкцию: 1) Открыть сайт geojson.io, 2) Нарисовать полигон с помощью инструмента рисования (Draw a polygon), 3) Скопировать сгенерированный JSON (справа) и прислать тебе в чат в виде текста или файла.

Текущее состояние настроек:
%s

Отвечай в дружелюбном тоне.`, string(state))

	messages := []chatMessage{
		{Role: "system", Content: systemPrompt},
	}

	var activeHistory []profiles.MCPMessage
	for _, msg := range profile.MCPHistory {
		if a.ttlHours > 0 && !msg.CreatedAt.IsZero() && time.Since(msg.CreatedAt) > time.Duration(a.ttlHours)*time.Hour {
			continue
		}
		activeHistory = append(activeHistory, msg)
	}
	profile.MCPHistory = activeHistory

	for _, msg := range profile.MCPHistory {
		messages = append(messages, chatMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	messages = append(messages, chatMessage{Role: "user", Content: text})

	tools := []chatTool{
		{
			Type: "function",
			Function: chatFunction{
				Name:        "set_filtering_status",
				Description: "Включает или выключает общую фильтрацию объявлений. Если фильтрация выключена, все объявления будут проходить независимо от правил.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"enabled": map[string]any{
							"type":        "boolean",
							"description": "true - включить фильтрацию (обычный режим), false - выключить (пропускать все объявления)",
						},
					},
					"required": []string{"enabled"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "update_preferences",
				Description: "Полностью обновляет параметры поиска квартир (бюджет, площадь, ремонт, локации, стоп-слова и т.д.).",
				Parameters: openaipreferences.PreferencesSchema(),
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "resolve_address",
				Description: "Находит географические координаты (latitude, longitude) и имя места для текстового адреса или названия объекта (например, 'Батуми, проспект Руставели 10').",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"address": map[string]any{
							"type":        "string",
							"description": "Адрес или название места",
						},
					},
					"required": []string{"address"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "calculate_distance",
				Description: "Вычисляет расстояние в метрах по прямой между двумя точками на Земле по их координатам.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"lat1": map[string]any{"type": "number", "description": "Широта первой точки"},
						"lon1": map[string]any{"type": "number", "description": "Долгота первой точки"},
						"lat2": map[string]any{"type": "number", "description": "Широта второй точки"},
						"lon2": map[string]any{"type": "number", "description": "Долгота второй точки"},
					},
					"required": []string{"lat1", "lon1", "lat2", "lon2"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "update_search_area",
				Description: "Устанавливает или обновляет полигон зоны поиска (search area) напрямую, используя предоставленный GeoJSON (Polygon или MultiPolygon). Используй это, если пользователь нарисовал зону или прислал валидный GeoJSON.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"geojson": map[string]any{
							"type":        "string",
							"description": "Валидная GeoJSON-строка с объектом Polygon или MultiPolygon",
						},
					},
					"required": []string{"geojson"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "search_local_listings",
				Description: "Ищет объявления по локальной истории (проверенным квартирам) по различным фильтрам: статус, текстовый поисковый запрос (включая описание, особенности ремонта, детали фото), диапазон цен и район/локация.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status": map[string]any{
							"type":        "string",
							"description": "Фильтр по статусу (например: 'passed', 'rejected_ai', 'rejected_location', 'rejected_basic', 'duplicate'). Если не указан, ищутся все объявления.",
						},
						"query": map[string]any{
							"type":        "string",
							"description": "Поисковый запрос для текстового поиска по описанию, заголовку, району или деталям ремонта/дизайна (сравнивается с текстом объявления и отчетом).",
						},
						"min_price": map[string]any{
							"type":        "integer",
							"description": "Минимальная цена в USD.",
						},
						"max_price": map[string]any{
							"type":        "integer",
							"description": "Максимальная цена в USD.",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "Название района или адрес для поиска по локации.",
						},
						"limit": map[string]any{
							"type":        "integer",
							"description": "Максимальное количество возвращаемых результатов (по умолчанию 10).",
						},
					},
				},
			},
		},
	}

	reqBody := chatRequest{
		Model:    a.model,
		Messages: messages,
		Tools:    tools,
	}

	respMsg, err := a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, nil, err
	}

	if len(respMsg.ToolCalls) == 0 {
		now := time.Now()
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: text, CreatedAt: now})
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
		if len(profile.MCPHistory) > 50 {
			profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-50:]
		}
		if err := a.store.UpsertProfile(ctx, profile); err != nil {
			return "", nil, nil, fmt.Errorf("failed to save mcp history: %w", err)
		}
		return respMsg.Content, nil, nil, nil
	}

	var prefsChanged *domain.UserPreferences
	var areaChanged *domain.GeoSearchArea

	// Apply tool calls
	messages = append(messages, respMsg)
	for _, tc := range respMsg.ToolCalls {
		var toolResult string
		switch tc.Function.Name {
		case "set_filtering_status":
			var args struct {
				Enabled bool `json:"enabled"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			disabled := !args.Enabled
			profile.FilterDisabled = &disabled
			err = a.store.UpsertProfile(ctx, profile)
			if err == nil {
				toolResult = "success"
			} else {
				toolResult = err.Error()
			}
		case "update_preferences":
			var newPrefs domain.UserPreferences
			err := json.Unmarshal([]byte(tc.Function.Arguments), &newPrefs)
			if err != nil {
				toolResult = "error: failed to parse preferences JSON"
			} else {
				profile.Preferences = &newPrefs
				prefsChanged = &newPrefs
				toolResult = "success"
			}
		case "resolve_address":
			var args struct {
				Address string `json:"address"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if a.geocoder == nil {
				toolResult = "error: geocoder is not configured"
			} else {
				loc, err := a.geocoder.Resolve(ctx, args.Address, 0)
				if err != nil {
					toolResult = "error: " + err.Error()
				} else if loc.Latitude == 0 && loc.Longitude == 0 {
					toolResult = "error: address not found"
				} else {
					resBytes, _ := json.Marshal(map[string]any{
						"name":      loc.Name,
						"latitude":  loc.Latitude,
						"longitude": loc.Longitude,
					})
					toolResult = string(resBytes)
				}
			}
		case "calculate_distance":
			var args struct {
				Lat1 float64 `json:"lat1"`
				Lon1 float64 `json:"lon1"`
				Lat2 float64 `json:"lat2"`
				Lon2 float64 `json:"lon2"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			dist := haversineDistance(args.Lat1, args.Lon1, args.Lat2, args.Lon2)
			resBytes, _ := json.Marshal(map[string]any{
				"distance_meters": int(dist),
			})
			toolResult = string(resBytes)
		case "update_search_area":
			var args struct {
				GeoJSON string `json:"geojson"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			area, err := parseGeoJSONArea(args.GeoJSON)
			if err != nil {
				toolResult = "error: " + err.Error()
			} else {
				areaChanged = area
				toolResult = "success"
			}
		case "search_local_listings":
			var args struct {
				Status   string `json:"status"`
				Query    string `json:"query"`
				MinPrice int    `json:"min_price"`
				MaxPrice int    `json:"max_price"`
				Location string `json:"location"`
				Limit    int    `json:"limit"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			filter := domain.EvaluationSearchFilter{
				Status:   args.Status,
				Query:    args.Query,
				MinPrice: args.MinPrice,
				MaxPrice: args.MaxPrice,
				Location: args.Location,
				Limit:    args.Limit,
			}
			records, err := a.store.SearchEvaluations(ctx, profileID, filter)
			if err != nil {
				toolResult = "error: " + err.Error()
			} else {
				resBytes, _ := json.Marshal(records)
				toolResult = string(resBytes)
			}
		default:
			toolResult = "unknown function"
		}

		messages = append(messages, chatMessage{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    toolResult,
		})
	}

	// Final request to get summary
	reqBody.Messages = messages
	reqBody.Tools = nil

	respMsg, err = a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, nil, err
	}

	// Update sliding window history
	now := time.Now()
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: text, CreatedAt: now})
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
	if len(profile.MCPHistory) > 50 {
		profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-50:]
	}
	
	if err := a.store.UpsertProfile(ctx, profile); err != nil {
		return "", nil, nil, fmt.Errorf("failed to save mcp history: %w", err)
	}

	return respMsg.Content, prefsChanged, areaChanged, nil
}

func (a *Agent) ProcessFeedback(ctx context.Context, profileID string, listing domain.ApartmentListing, feedbackText string) (string, *domain.UserPreferences, *domain.GeoSearchArea, error) {
	profile, ok, err := a.store.GetProfile(ctx, profileID)
	if err != nil {
		return "", nil, nil, err
	}
	if !ok {
		return "Профиль не найден", nil, nil, nil
	}

	var searchAreaJSON any
	if saStore, ok := a.store.(interface {
		LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error)
	}); ok {
		if geoJSON, ok, err := saStore.LatestSearchAreaGeoJSON(ctx, profileID); err == nil && ok {
			var parsed any
			if err := json.Unmarshal([]byte(geoJSON), &parsed); err == nil {
				searchAreaJSON = parsed
			} else {
				searchAreaJSON = geoJSON
			}
		}
	}

	state, err := json.MarshalIndent(map[string]any{
		"FilterDisabled": profiles.FilterDisabled(profile),
		"Preferences":    profile.Preferences,
		"SearchArea":     searchAreaJSON,
	}, "", "  ")
	if err != nil {
		return "", nil, nil, err
	}

	systemPrompt := fmt.Sprintf(`Ты - персональный ассистент по поиску квартир (MCP Agent). Пользователь ответил на отчет об объявлении о квартире с отзывом. Твоя цель — помочь пользователю уточнить фильтры, основываясь на его отзыве и данных объявления.
Ты можешь:
1. Рассказывать пользователю, какие у него сейчас стоят настройки и как ты видишь его идеальную квартиру.
2. Обсуждать желаемый ремонт, район, бюджет, удобства и помогать сформулировать четкие критерии.
3. Изменять фильтры с помощью инструмента update_preferences, НО только после того, как согласуешь изменения с пользователем, или если пользователь прямо попросил обновить фильтр.
4. Включать/выключать фильтрацию с помощью set_filtering_status.
5. Работать с географией: находить координаты по адресу (resolve_address), вычислять расстояние в метрах (calculate_distance) и обновлять зону поиска напрямую с помощью GeoJSON (update_search_area).

Веди диалог живо, эмпатично и профессионально. Задавай уточняющие вопросы, если пожелания размыты (например, "хочу бабушкин вариант" -> предложи добавить "soviet" в reject_rules).

ВАЖНО:
- Используй Markdown разметку для оформления (списки, **жирный текст** и т.д.).
- Не упоминай технические ключи из JSON (например, location_preferences, reject_rules). Если выводишь список фильтров или локаций, перечисляй их естественным, человекочитаемым языком.
- Если в состоянии настроек заполнен ключ "SearchArea" (даже если текстовые предпочтения по районам пусты), это ОЗНАЧАЕТ, что локация для поиска задана (пользователь ограничил поиск гео-зоной на карте). В этом случае НИКОГДА не пиши, что локация не задана или что предпочтений по районам нет. Указывай, что поиск идет в границах заданной области/зоны поиска на карте.
- Если пользователь хочет сам нарисовать зону поиска, обязательно дай ему пошаговую инструкцию: 1) Открыть сайт geojson.io, 2) Нарисовать полигон с помощью инструмента рисования (Draw a polygon), 3) Скопировать сгенерированный JSON (справа) и прислать тебе в чат в виде текста или файла.

Текущее состояние настроек:
%s

Отвечай в дружелюбном тоне.`, string(state))

	messages := []chatMessage{
		{Role: "system", Content: systemPrompt},
	}

	var activeHistory []profiles.MCPMessage
	for _, msg := range profile.MCPHistory {
		if a.ttlHours > 0 && !msg.CreatedAt.IsZero() && time.Since(msg.CreatedAt) > time.Duration(a.ttlHours)*time.Hour {
			continue
		}
		activeHistory = append(activeHistory, msg)
	}
	profile.MCPHistory = activeHistory

	for _, msg := range profile.MCPHistory {
		messages = append(messages, chatMessage{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	listingJSON, _ := json.MarshalIndent(listing, "", "  ")
	userContent := fmt.Sprintf("Контекст: Я ответил на следующее объявление:\n%s\n\nМой отзыв: %s", string(listingJSON), feedbackText)
	messages = append(messages, chatMessage{Role: "user", Content: userContent})

	tools := []chatTool{
		{
			Type: "function",
			Function: chatFunction{
				Name:        "set_filtering_status",
				Description: "Включает или выключает общую фильтрацию объявлений. Если фильтрация выключена, все объявления будут проходить независимо от правил.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"enabled": map[string]any{
							"type":        "boolean",
							"description": "true - включить фильтрацию (обычный режим), false - выключить (пропускать все объявления)",
						},
					},
					"required": []string{"enabled"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "update_preferences",
				Description: "Полностью обновляет параметры поиска квартир (бюджет, площадь, ремонт, локации, стоп-слова и т.д.).",
				Parameters: openaipreferences.PreferencesSchema(),
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "resolve_address",
				Description: "Находит географические координаты (latitude, longitude) и имя места для текстового адреса или названия объекта (например, 'Батуми, проспект Руставели 10').",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"address": map[string]any{
							"type":        "string",
							"description": "Адрес или название места",
						},
					},
					"required": []string{"address"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "calculate_distance",
				Description: "Вычисляет расстояние в метрах по прямой между двумя точками на Земле по их координатам.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"lat1": map[string]any{"type": "number", "description": "Широта первой точки"},
						"lon1": map[string]any{"type": "number", "description": "Долгота первой точки"},
						"lat2": map[string]any{"type": "number", "description": "Широта второй точки"},
						"lon2": map[string]any{"type": "number", "description": "Долгота второй точки"},
					},
					"required": []string{"lat1", "lon1", "lat2", "lon2"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "update_search_area",
				Description: "Устанавливает или обновляет полигон зоны поиска (search area) напрямую, используя предоставленный GeoJSON (Polygon или MultiPolygon). Используй это, если пользователь нарисовал зону или прислал валидный GeoJSON.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"geojson": map[string]any{
							"type":        "string",
							"description": "Валидная GeoJSON-строка с объектом Polygon или MultiPolygon",
						},
					},
					"required": []string{"geojson"},
				},
			},
		},
		{
			Type: "function",
			Function: chatFunction{
				Name:        "search_local_listings",
				Description: "Ищет объявления по локальной истории (проверенным квартирам) по различным фильтрам: статус, текстовый поисковый запрос (включая описание, особенности ремонта, детали фото), диапазон цен и район/локация.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status": map[string]any{
							"type":        "string",
							"description": "Фильтр по статусу (например: 'passed', 'rejected_ai', 'rejected_location', 'rejected_basic', 'duplicate'). Если не указан, ищутся все объявления.",
						},
						"query": map[string]any{
							"type":        "string",
							"description": "Поисковый запрос для текстового поиска по описанию, заголовку, району или деталям ремонта/дизайна (сравнивается с текстом объявления и отчетом).",
						},
						"min_price": map[string]any{
							"type":        "integer",
							"description": "Минимальная цена в USD.",
						},
						"max_price": map[string]any{
							"type":        "integer",
							"description": "Максимальная цена в USD.",
						},
						"location": map[string]any{
							"type":        "string",
							"description": "Название района или адрес для поиска по локации.",
						},
						"limit": map[string]any{
							"type":        "integer",
							"description": "Максимальное количество возвращаемых результатов (по умолчанию 10).",
						},
					},
				},
			},
		},
	}

	reqBody := chatRequest{
		Model:    a.model,
		Messages: messages,
		Tools:    tools,
	}

	respMsg, err := a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, nil, err
	}

	if len(respMsg.ToolCalls) == 0 {
		now := time.Now()
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: "[Отзыв на объявление] " + feedbackText, CreatedAt: now})
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
		if len(profile.MCPHistory) > 50 {
			profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-50:]
		}
		if err := a.store.UpsertProfile(ctx, profile); err != nil {
			return "", nil, nil, fmt.Errorf("failed to save mcp history: %w", err)
		}
		return respMsg.Content, nil, nil, nil
	}

	var prefsChanged *domain.UserPreferences
	var areaChanged *domain.GeoSearchArea

	// Apply tool calls
	messages = append(messages, respMsg)
	for _, tc := range respMsg.ToolCalls {
		var toolResult string
		switch tc.Function.Name {
		case "set_filtering_status":
			var args struct {
				Enabled bool `json:"enabled"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			disabled := !args.Enabled
			profile.FilterDisabled = &disabled
			err = a.store.UpsertProfile(ctx, profile)
			if err == nil {
				toolResult = "success"
			} else {
				toolResult = err.Error()
			}
		case "update_preferences":
			var newPrefs domain.UserPreferences
			err := json.Unmarshal([]byte(tc.Function.Arguments), &newPrefs)
			if err != nil {
				toolResult = "error: failed to parse preferences JSON"
			} else {
				profile.Preferences = &newPrefs
				prefsChanged = &newPrefs
				toolResult = "success"
			}
		case "resolve_address":
			var args struct {
				Address string `json:"address"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			if a.geocoder == nil {
				toolResult = "error: geocoder is not configured"
			} else {
				loc, err := a.geocoder.Resolve(ctx, args.Address, 0)
				if err != nil {
					toolResult = "error: " + err.Error()
				} else if loc.Latitude == 0 && loc.Longitude == 0 {
					toolResult = "error: address not found"
				} else {
					resBytes, _ := json.Marshal(map[string]any{
						"name":      loc.Name,
						"latitude":  loc.Latitude,
						"longitude": loc.Longitude,
					})
					toolResult = string(resBytes)
				}
			}
		case "calculate_distance":
			var args struct {
				Lat1 float64 `json:"lat1"`
				Lon1 float64 `json:"lon1"`
				Lat2 float64 `json:"lat2"`
				Lon2 float64 `json:"lon2"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			dist := haversineDistance(args.Lat1, args.Lon1, args.Lat2, args.Lon2)
			resBytes, _ := json.Marshal(map[string]any{
				"distance_meters": int(dist),
			})
			toolResult = string(resBytes)
		case "update_search_area":
			var args struct {
				GeoJSON string `json:"geojson"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			area, err := parseGeoJSONArea(args.GeoJSON)
			if err != nil {
				toolResult = "error: " + err.Error()
			} else {
				areaChanged = area
				toolResult = "success"
			}
		case "search_local_listings":
			var args struct {
				Status   string `json:"status"`
				Query    string `json:"query"`
				MinPrice int    `json:"min_price"`
				MaxPrice int    `json:"max_price"`
				Location string `json:"location"`
				Limit    int    `json:"limit"`
			}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			filter := domain.EvaluationSearchFilter{
				Status:   args.Status,
				Query:    args.Query,
				MinPrice: args.MinPrice,
				MaxPrice: args.MaxPrice,
				Location: args.Location,
				Limit:    args.Limit,
			}
			records, err := a.store.SearchEvaluations(ctx, profileID, filter)
			if err != nil {
				toolResult = "error: " + err.Error()
			} else {
				resBytes, _ := json.Marshal(records)
				toolResult = string(resBytes)
			}
		default:
			toolResult = "unknown function"
		}

		messages = append(messages, chatMessage{
			Role:       "tool",
			ToolCallID: tc.ID,
			Name:       tc.Function.Name,
			Content:    toolResult,
		})
	}

	// Final request to get summary
	reqBody.Messages = messages
	reqBody.Tools = nil

	respMsg, err = a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, nil, err
	}

	// Update sliding window history
	now := time.Now()
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: "[Отзыв на объявление] " + feedbackText, CreatedAt: now})
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
	if len(profile.MCPHistory) > 10 {
		profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-10:]
	}
	
	if err := a.store.UpsertProfile(ctx, profile); err != nil {
		return "", nil, nil, fmt.Errorf("failed to save mcp history: %w", err)
	}

	return respMsg.Content, prefsChanged, areaChanged, nil
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
