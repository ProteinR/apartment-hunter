package mcpagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"apartment-hunter/internal/adapters/openaipreferences"
	"apartment-hunter/internal/cities"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
)

const (
	historyCompactionThreshold = 40
	historyKeptAfterCompaction = 16
	historyHardLimit           = 80
	summaryHistoryRole         = "summary"
	notificationMatchesOnly    = "matches_only"
	notificationAllReports     = "all_reports"
)

type SourceManager interface {
	ListSources(ctx context.Context, telegramChatID int64) ([]string, error)
	AddSources(ctx context.Context, telegramChatID int64, refs []string) (string, error)
	RemoveSources(ctx context.Context, telegramChatID int64, refs []string) (string, error)
}

func (a *Agent) SetSourceManager(sources SourceManager) {
	a.sources = sources
}

type conversationTurn struct {
	systemPrompt   string
	userContent    string
	historyContent string
	canListChats   bool
}

type conversationChanges struct {
	preferences *domain.UserPreferences
	searchArea  *domain.GeoSearchArea
}

func (a *Agent) converse(ctx context.Context, profile profiles.Profile, turn conversationTurn) (string, *domain.UserPreferences, *domain.GeoSearchArea, error) {
	profile.Channels = nil
	summary, history := splitHistorySummary(profile.MCPHistory)
	systemPrompt := turn.systemPrompt
	if summary != "" {
		systemPrompt += "\n\nКраткое содержание более раннего разговора (текущее состояние настроек выше важнее, если они расходятся):\n" + summary
	}
	messages := []chatMessage{{Role: "system", Content: systemPrompt}}
	for _, msg := range history {
		if a.historyMessageExpired(msg) {
			continue
		}
		messages = append(messages, chatMessage{Role: msg.Role, Content: msg.Content})
	}
	messages = append(messages, chatMessage{Role: "user", Content: turn.userContent})

	reqBody := chatRequest{
		Model:    a.model,
		Messages: messages,
		Tools:    a.conversationTools(profile, turn.canListChats),
	}
	respMsg, err := a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, nil, err
	}

	var changes conversationChanges
	for round := 1; len(respMsg.ToolCalls) > 0 && round <= maxAgentToolRounds; round++ {
		messages = append(messages, respMsg)
		for _, tc := range respMsg.ToolCalls {
			messages = append(messages, chatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    a.executeTool(ctx, &profile, tc, turn.canListChats, &changes),
			})
		}
		reqBody.Messages = messages
		if round >= maxAgentToolRounds {
			reqBody.ToolChoice = "none"
		}
		respMsg, err = a.makeChatRequest(ctx, reqBody)
		if err != nil {
			return "", nil, nil, err
		}
	}

	if strings.TrimSpace(respMsg.Content) == "" {
		respMsg.Content = emptyAgentReply
	}

	now := time.Now()
	profile.MCPHistory = append(profile.MCPHistory,
		profiles.MCPMessage{Role: "user", Content: turn.historyContent, CreatedAt: now},
		profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now},
	)
	profile.MCPHistory = a.compactHistory(ctx, profile.MCPHistory)

	if err := a.store.UpsertProfile(ctx, profiles.Profile{ID: profile.ID, MCPHistory: profile.MCPHistory}); err != nil {
		return "", nil, nil, fmt.Errorf("failed to save mcp history: %w", err)
	}
	return respMsg.Content, changes.preferences, changes.searchArea, nil
}

func (a *Agent) historyMessageExpired(msg profiles.MCPMessage) bool {
	return a.ttlHours > 0 && !msg.CreatedAt.IsZero() && time.Since(msg.CreatedAt) > time.Duration(a.ttlHours)*time.Hour
}

func splitHistorySummary(history []profiles.MCPMessage) (string, []profiles.MCPMessage) {
	summary := ""
	var rest []profiles.MCPMessage
	for _, msg := range history {
		if msg.Role == summaryHistoryRole {
			summary = msg.Content
			continue
		}
		rest = append(rest, msg)
	}
	return summary, rest
}

func (a *Agent) compactHistory(ctx context.Context, history []profiles.MCPMessage) []profiles.MCPMessage {
	summary, rest := splitHistorySummary(history)
	var stale, fresh []profiles.MCPMessage
	for _, msg := range rest {
		if a.historyMessageExpired(msg) {
			stale = append(stale, msg)
		} else {
			fresh = append(fresh, msg)
		}
	}
	if len(fresh) > historyCompactionThreshold {
		cut := len(fresh) - historyKeptAfterCompaction
		stale = append(stale, fresh[:cut]...)
		fresh = fresh[cut:]
	}
	if len(stale) == 0 {
		return history
	}
	newSummary, err := a.summarizeConversation(ctx, summary, stale)
	if err != nil {
		fmt.Fprintf(os.Stderr, "MCP history compaction failed: %v\n", err)
		if len(rest) > historyHardLimit {
			rest = rest[len(rest)-historyHardLimit:]
		}
		return withSummary(summary, rest)
	}
	return withSummary(newSummary, fresh)
}

func withSummary(summary string, messages []profiles.MCPMessage) []profiles.MCPMessage {
	compacted := make([]profiles.MCPMessage, 0, len(messages)+1)
	if strings.TrimSpace(summary) != "" {
		compacted = append(compacted, profiles.MCPMessage{Role: summaryHistoryRole, Content: summary, CreatedAt: time.Now()})
	}
	return append(compacted, messages...)
}

func (a *Agent) summarizeConversation(ctx context.Context, previousSummary string, messages []profiles.MCPMessage) (string, error) {
	var transcript strings.Builder
	if previousSummary != "" {
		transcript.WriteString("Прежнее краткое содержание:\n")
		transcript.WriteString(previousSummary)
		transcript.WriteString("\n\n")
	}
	transcript.WriteString("Новые сообщения:\n")
	for _, msg := range messages {
		speaker := "Пользователь"
		if msg.Role == "assistant" {
			speaker = "Ассистент"
		}
		transcript.WriteString(speaker + ": " + msg.Content + "\n")
	}
	reply, err := a.makeChatRequest(ctx, chatRequest{
		Model: a.model,
		Messages: []chatMessage{
			{Role: "system", Content: conversationSummaryPrompt},
			{Role: "user", Content: transcript.String()},
		},
	})
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(reply.Content)
	if summary == "" {
		return "", fmt.Errorf("empty conversation summary")
	}
	return summary, nil
}

const conversationSummaryPrompt = `Ты сжимаешь историю переписки пользователя с ассистентом по поиску квартир, чтобы ассистент мог продолжить разговор без полной истории.
Объедини прежнее краткое содержание с новыми сообщениями в одно краткое содержание на русском, не длиннее 1500 символов.
Сохрани: что пользователь хочет и не хочет в квартире, о чем договорились, что ассистент уже сделал или пообещал, открытые вопросы и на чем остановился разговор.
Не пересказывай приветствия и повторы. Пиши фактами, списком.`

func (a *Agent) conversationState(ctx context.Context, profile profiles.Profile) (string, error) {
	var searchAreaJSON any
	if saStore, ok := a.store.(interface {
		LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error)
	}); ok {
		if geoJSON, ok, err := saStore.LatestSearchAreaGeoJSON(ctx, profile.ID); err == nil && ok {
			var parsed any
			if err := json.Unmarshal([]byte(geoJSON), &parsed); err == nil {
				searchAreaJSON = parsed
			} else {
				searchAreaJSON = geoJSON
			}
		}
	}
	notificationMode := notificationMatchesOnly
	if profiles.DebugModeEnabled(profile) {
		notificationMode = notificationAllReports
	}
	state, err := json.MarshalIndent(map[string]any{
		"FilterDisabled":   profiles.FilterDisabled(profile),
		"Preferences":      profile.Preferences,
		"SearchArea":       searchAreaJSON,
		"ConnectedSources": profile.Channels,
		"NotificationMode": notificationMode,
	}, "", "  ")
	if err != nil {
		return "", err
	}
	return string(state), nil
}

func (a *Agent) mayManageSources(profile profiles.Profile) bool {
	return a.sources != nil && profile.TelegramChatID != 0
}

func functionTool(name, description string, parameters map[string]any) chatTool {
	return chatTool{Type: "function", Function: chatFunction{Name: name, Description: description, Parameters: parameters}}
}

func sourceRefsParameters(description string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"sources": map[string]any{
				"type":        "array",
				"items":       map[string]any{"type": "string"},
				"description": description,
			},
		},
		"required": []string{"sources"},
	}
}

func (a *Agent) conversationTools(profile profiles.Profile, canListChats bool) []chatTool {
	tools := []chatTool{
		functionTool("set_filtering_status", "Включает или выключает общую фильтрацию объявлений. Если фильтрация выключена, все объявления будут проходить независимо от правил.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"enabled": map[string]any{
					"type":        "boolean",
					"description": "true - включить фильтрацию (обычный режим), false - выключить (пропускать все объявления)",
				},
			},
			"required": []string{"enabled"},
		}),
		functionTool("set_notification_mode", "Выбирает, какие объявления присылать пользователю: matches_only — только прошедшие фильтры; all_reports — отчет по каждому обработанному объявлению с причиной, почему оно прошло или нет.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"mode": map[string]any{
					"type": "string",
					"enum": []string{notificationMatchesOnly, notificationAllReports},
				},
			},
			"required": []string{"mode"},
		}),
		functionTool("update_preferences", "Обновляет параметры поиска квартир (город, бюджет, площадь, ремонт, локации, стоп-слова и т.д.) и сохраняет профиль поиска. Поля, которые не передал, остаются как были.", openaipreferences.PreferencesSchema()),
		functionTool("resolve_address", "Находит географические координаты (latitude, longitude) и имя места для текстового адреса или названия объекта (например, 'проспект Руставели 10'; город поиска пользователя подставляется автоматически).", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"address": map[string]any{"type": "string", "description": "Адрес или название места"},
				"city": map[string]any{
					"type":        "string",
					"enum":        []string{"batumi", "tbilisi"},
					"description": "Город, в котором искать адрес. Если не указан, используется город поиска пользователя.",
				},
			},
			"required": []string{"address"},
		}),
		functionTool("calculate_distance", "Вычисляет расстояние в метрах по прямой между двумя точками на Земле по их координатам.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"lat1": map[string]any{"type": "number", "description": "Широта первой точки"},
				"lon1": map[string]any{"type": "number", "description": "Долгота первой точки"},
				"lat2": map[string]any{"type": "number", "description": "Широта второй точки"},
				"lon2": map[string]any{"type": "number", "description": "Долгота второй точки"},
			},
			"required": []string{"lat1", "lon1", "lat2", "lon2"},
		}),
		functionTool("update_search_area", "Устанавливает или обновляет полигон зоны поиска (search area) напрямую, используя предоставленный GeoJSON (Polygon или MultiPolygon). Используй это, если пользователь нарисовал зону или прислал валидный GeoJSON.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"geojson": map[string]any{"type": "string", "description": "Валидная GeoJSON-строка с объектом Polygon или MultiPolygon"},
			},
			"required": []string{"geojson"},
		}),
		functionTool("search_local_listings", "Ищет объявления по локальной истории (проверенным квартирам) по различным фильтрам: статус, текстовый поисковый запрос (включая описание, особенности ремонта, детали фото), диапазон цен и район/локация.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":    map[string]any{"type": "string", "description": "Фильтр по статусу (например: 'passed', 'rejected_ai', 'rejected_location', 'rejected_basic', 'duplicate'). Если не указан, ищутся все объявления."},
				"query":     map[string]any{"type": "string", "description": "Поисковый запрос для текстового поиска по описанию, заголовку, району или деталям ремонта/дизайна (сравнивается с текстом объявления и отчетом)."},
				"min_price": map[string]any{"type": "integer", "description": "Минимальная цена в USD."},
				"max_price": map[string]any{"type": "integer", "description": "Максимальная цена в USD."},
				"location":  map[string]any{"type": "string", "description": "Название района или адрес для поиска по локации."},
				"limit":     map[string]any{"type": "integer", "description": "Максимальное количество возвращаемых результатов (по умолчанию 10)."},
			},
		}),
		functionTool("get_listing_evaluation_report", "Возвращает полный сохраненный отчет по одному объявлению: само объявление, источник, статус, причины отклонения, AI-инсайты, итоговый текст отчета и ошибку. Используй, когда пользователь спрашивает, почему конкретное объявление прошло или не прошло фильтры. message_id — это replied_message_id из контекста отзыва, ID сообщения бота с отчетом или ID исходного сообщения в канале.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"message_id":       map[string]any{"type": "integer", "description": "ID сообщения бота с отчетом или исходного сообщения объявления."},
				"telegram_chat_id": map[string]any{"type": "integer", "description": "Telegram chat id пользователя. Если не указан, используется текущий chat id профиля."},
			},
			"required": []string{"message_id"},
		}),
	}
	if profile.Preferences == nil {
		tools = slices.DeleteFunc(tools, func(tool chatTool) bool { return tool.Function.Name == "update_search_area" })
	}
	if a.mayManageSources(profile) {
		tools = append(tools,
			functionTool("list_sources", "Возвращает актуальный список Telegram-каналов и групп, из которых сейчас читаются объявления для пользователя.", map[string]any{"type": "object", "properties": map[string]any{}}),
			functionTool("add_sources", "Подключает публичные Telegram-каналы или группы с объявлениями об аренде как источники. Результат сообщает по каждому, подключен ли он и может ли бот его читать.", sourceRefsParameters("Ссылки t.me/name или @username, как их прислал пользователь")),
			functionTool("remove_sources", "Отключает источники объявлений.", sourceRefsParameters("@username или ссылки t.me/name источников, которые нужно отключить")),
		)
	}
	if canListChats {
		tools = append(tools, functionTool("list_available_telegram_chats", "Возвращает public Telegram-каналы и группы, на которые уже подписан userbot-аккаунт. Используй для выбора релевантных источников объявлений, которые можно предложить подключить через add_sources.", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit": map[string]any{"type": "integer", "description": "Максимальное число dialogs для просмотра, по умолчанию 100, максимум 200."},
			},
		}))
	}
	return tools
}

func marshalToolResult(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "error: " + err.Error()
	}
	return string(data)
}

func (a *Agent) executeTool(ctx context.Context, profile *profiles.Profile, tc toolCall, canListChats bool, changes *conversationChanges) string {
	arguments := []byte(tc.Function.Arguments)
	switch tc.Function.Name {
	case "set_filtering_status":
		var args struct {
			Enabled bool `json:"enabled"`
		}
		json.Unmarshal(arguments, &args)
		disabled := !args.Enabled
		profile.FilterDisabled = &disabled
		if err := a.store.UpsertProfile(ctx, profiles.Profile{ID: profile.ID, FilterDisabled: &disabled}); err != nil {
			return err.Error()
		}
		return "success"
	case "set_notification_mode":
		var args struct {
			Mode string `json:"mode"`
		}
		json.Unmarshal(arguments, &args)
		if args.Mode != notificationMatchesOnly && args.Mode != notificationAllReports {
			return "error: mode must be matches_only or all_reports"
		}
		allReports := args.Mode == notificationAllReports
		profile.DebugMode = &allReports
		if err := a.store.UpsertProfile(ctx, profiles.Profile{ID: profile.ID, DebugMode: &allReports}); err != nil {
			return "error: " + err.Error()
		}
		return "success"
	case "update_preferences":
		newPrefs, err := mergePreferenceUpdate(profile.Preferences, tc.Function.Arguments)
		if err != nil {
			return "error: failed to parse preferences JSON"
		}
		profile.Preferences = &newPrefs
		changes.preferences = &newPrefs
		return "success"
	case "resolve_address":
		var args struct {
			Address string `json:"address"`
			City    string `json:"city"`
		}
		json.Unmarshal(arguments, &args)
		if a.geocoder == nil {
			return "error: geocoder is not configured"
		}
		loc, err := a.geocoder.Resolve(ctx, cities.ForKey(args.City, profileSearchCity(*profile)).QueryWithRegion(args.Address), 0)
		if err != nil {
			return "error: " + err.Error()
		}
		if loc.Latitude == 0 && loc.Longitude == 0 {
			return "error: address not found"
		}
		return marshalToolResult(map[string]any{"name": loc.Name, "latitude": loc.Latitude, "longitude": loc.Longitude})
	case "calculate_distance":
		var args struct {
			Lat1 float64 `json:"lat1"`
			Lon1 float64 `json:"lon1"`
			Lat2 float64 `json:"lat2"`
			Lon2 float64 `json:"lon2"`
		}
		json.Unmarshal(arguments, &args)
		return marshalToolResult(map[string]any{"distance_meters": int(haversineDistance(args.Lat1, args.Lon1, args.Lat2, args.Lon2))})
	case "update_search_area":
		if profile.Preferences == nil {
			return "error: confirm and save the search profile with update_preferences first"
		}
		var args struct {
			GeoJSON string `json:"geojson"`
		}
		json.Unmarshal(arguments, &args)
		area, err := parseGeoJSONArea(args.GeoJSON)
		if err != nil {
			return "error: " + err.Error()
		}
		changes.searchArea = area
		return "success"
	case "search_local_listings":
		var args struct {
			Status   string `json:"status"`
			Query    string `json:"query"`
			MinPrice int    `json:"min_price"`
			MaxPrice int    `json:"max_price"`
			Location string `json:"location"`
			Limit    int    `json:"limit"`
		}
		json.Unmarshal(arguments, &args)
		records, err := a.store.SearchEvaluations(ctx, profile.ID, domain.EvaluationSearchFilter{
			Status:   args.Status,
			Query:    args.Query,
			MinPrice: args.MinPrice,
			MaxPrice: args.MaxPrice,
			Location: args.Location,
			Limit:    args.Limit,
		})
		if err != nil {
			return "error: " + err.Error()
		}
		return marshalToolResult(records)
	case "get_listing_evaluation_report":
		var args struct {
			MessageID      int   `json:"message_id"`
			TelegramChatID int64 `json:"telegram_chat_id"`
		}
		json.Unmarshal(arguments, &args)
		chatID := args.TelegramChatID
		if chatID == 0 {
			chatID = profile.TelegramChatID
		}
		record, ok, err := a.store.GetEvaluationByTelegramMessage(ctx, profile.ID, chatID, args.MessageID)
		if err != nil {
			return "error: " + err.Error()
		}
		if !ok {
			return "not found"
		}
		return marshalToolResult(record)
	case "list_sources":
		if !a.mayManageSources(*profile) {
			return "unknown function"
		}
		sources, err := a.sources.ListSources(ctx, profile.TelegramChatID)
		if err != nil {
			return "error: " + err.Error()
		}
		if len(sources) == 0 {
			return "no sources connected"
		}
		return strings.Join(sources, "\n")
	case "add_sources", "remove_sources":
		if !a.mayManageSources(*profile) {
			return "unknown function"
		}
		var args struct {
			Sources []string `json:"sources"`
		}
		json.Unmarshal(arguments, &args)
		manage := a.sources.AddSources
		if tc.Function.Name == "remove_sources" {
			manage = a.sources.RemoveSources
		}
		result, err := manage(ctx, profile.TelegramChatID, args.Sources)
		if err != nil {
			return "error: " + err.Error()
		}
		return result
	case "list_available_telegram_chats":
		var args struct {
			Limit int `json:"limit"`
		}
		json.Unmarshal(arguments, &args)
		if !canListChats {
			return "unknown function"
		}
		if a.chatLister == nil {
			return "error: telegram chat lister is not configured"
		}
		chats, err := a.chatLister.ListAvailableChats(ctx, args.Limit)
		if err != nil {
			return "error: " + err.Error()
		}
		return marshalToolResult(chats)
	default:
		return "unknown function"
	}
}
