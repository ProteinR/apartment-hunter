package mcpagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
}

type Agent struct {
	apiKey   string
	model    string
	apiURL   string
	ttlHours int
	client   *http.Client
	store    ProfileStore
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

func (a *Agent) ProcessCommand(ctx context.Context, profileID string, text string) (string, *domain.UserPreferences, error) {
	profile, ok, err := a.store.GetProfile(ctx, profileID)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "Профиль не найден", nil, nil
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
		return "", nil, err
	}

	systemPrompt := fmt.Sprintf(`Ты - персональный ассистент по поиску квартир (MCP Agent). Твоя цель — помочь пользователю сформировать идеальные фильтры для поиска жилья.
Ты можешь:
1. Рассказывать пользователю, какие у него сейчас стоят настройки и как ты видишь его идеальную квартиру.
2. Обсуждать желаемый ремонт, район, бюджет, удобства и помогать сформулировать четкие критерии.
3. Изменять фильтры с помощью инструмента update_preferences, НО только после того, как согласуешь изменения с пользователем, или если пользователь прямо попросил обновить фильтр.
4. Включать/выключать фильтрацию с помощью set_filtering_status.

Веди диалог живо, эмпатично и профессионально. Задавай уточняющие вопросы, если пожелания размыты (например, "хочу бабушкин вариант" -> предложи добавить "soviet" в reject_rules).

ВАЖНО:
- Используй Markdown разметку для оформления (списки, **жирный текст** и т.д.).
- Не упоминай технические ключи из JSON (например, location_preferences, reject_rules). Если выводишь список фильтров или локаций, перечисляй их естественным, человекочитаемым языком.

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
	}

	reqBody := chatRequest{
		Model:    a.model,
		Messages: messages,
		Tools:    tools,
	}

	respMsg, err := a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, err
	}

	if len(respMsg.ToolCalls) == 0 {
		now := time.Now()
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: text, CreatedAt: now})
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
		if len(profile.MCPHistory) > 50 {
			profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-50:]
		}
		if err := a.store.UpsertProfile(ctx, profile); err != nil {
			return "", nil, fmt.Errorf("failed to save mcp history: %w", err)
		}
		return respMsg.Content, nil, nil
	}

	var prefsChanged *domain.UserPreferences

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
				err = a.store.UpsertProfile(ctx, profile)
				if err == nil {
					toolResult = "success"
				} else {
					toolResult = err.Error()
				}
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
		return "", nil, err
	}

	// Update sliding window history
	now := time.Now()
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: text, CreatedAt: now})
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
	if len(profile.MCPHistory) > 50 {
		profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-50:]
	}
	
	if err := a.store.UpsertProfile(ctx, profile); err != nil {
		return "", nil, fmt.Errorf("failed to save mcp history: %w", err)
	}

	return respMsg.Content, prefsChanged, nil
}

func (a *Agent) ProcessFeedback(ctx context.Context, profileID string, listing domain.ApartmentListing, feedbackText string) (string, *domain.UserPreferences, error) {
	profile, ok, err := a.store.GetProfile(ctx, profileID)
	if err != nil {
		return "", nil, err
	}
	if !ok {
		return "Профиль не найден", nil, nil
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
		return "", nil, err
	}

	systemPrompt := fmt.Sprintf(`Ты - персональный ассистент по поиску квартир (MCP Agent). Пользователь ответил на отчет об объявлении о квартире с отзывом. Твоя цель — помочь пользователю уточнить фильтры, основываясь на его отзыве и данных объявления.
Ты можешь:
1. Рассказывать пользователю, какие у него сейчас стоят настройки и как ты видишь его идеальную квартиру.
2. Обсуждать желаемый ремонт, район, бюджет, удобства и помогать сформулировать четкие критерии.
3. Изменять фильтры с помощью инструмента update_preferences, НО только после того, как согласуешь изменения с пользователем, или если пользователь прямо попросил обновить фильтр.
4. Включать/выключать фильтрацию с помощью set_filtering_status.

Веди диалог живо, эмпатично и профессионально. Задавай уточняющие вопросы, если пожелания размыты (например, "хочу бабушкин вариант" -> предложи добавить "soviet" в reject_rules).

ВАЖНО:
- Используй Markdown разметку для оформления (списки, **жирный текст** и т.д.).
- Не упоминай технические ключи из JSON (например, location_preferences, reject_rules). Если выводишь список фильтров или локаций, перечисляй их естественным, человекочитаемым языком.

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
	}

	reqBody := chatRequest{
		Model:    a.model,
		Messages: messages,
		Tools:    tools,
	}

	respMsg, err := a.makeChatRequest(ctx, reqBody)
	if err != nil {
		return "", nil, err
	}

	if len(respMsg.ToolCalls) == 0 {
		now := time.Now()
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: "[Отзыв на объявление] " + feedbackText, CreatedAt: now})
		profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
		if len(profile.MCPHistory) > 50 {
			profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-50:]
		}
		if err := a.store.UpsertProfile(ctx, profile); err != nil {
			return "", nil, fmt.Errorf("failed to save mcp history: %w", err)
		}
		return respMsg.Content, nil, nil
	}

	var prefsChanged *domain.UserPreferences

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
				err = a.store.UpsertProfile(ctx, profile)
				if err == nil {
					toolResult = "success"
				} else {
					toolResult = err.Error()
				}
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
		return "", nil, err
	}

	// Update sliding window history
	now := time.Now()
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "user", Content: "[Отзыв на объявление] " + feedbackText, CreatedAt: now})
	profile.MCPHistory = append(profile.MCPHistory, profiles.MCPMessage{Role: "assistant", Content: respMsg.Content, CreatedAt: now})
	if len(profile.MCPHistory) > 10 {
		profile.MCPHistory = profile.MCPHistory[len(profile.MCPHistory)-10:]
	}
	
	if err := a.store.UpsertProfile(ctx, profile); err != nil {
		return "", nil, fmt.Errorf("failed to save mcp history: %w", err)
	}

	return respMsg.Content, prefsChanged, nil
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
