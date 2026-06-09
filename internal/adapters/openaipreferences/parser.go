package openaipreferences

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"apartment-hunter/internal/domain"
)

const (
	defaultAPIURL = "https://openrouter.ai/api/v1/chat/completions"
	defaultModel  = "openai/gpt-4o-mini"
)

type Parser struct {
	apiKey string
	model  string
	apiURL string
	client *http.Client
}

type Option func(*Parser)

func WithHTTPClient(client *http.Client) Option {
	return func(p *Parser) {
		p.client = client
	}
}

func WithAPIURL(apiURL string) Option {
	return func(p *Parser) {
		p.apiURL = strings.TrimSpace(apiURL)
	}
}

func WithModel(model string) Option {
	return func(p *Parser) {
		p.model = strings.TrimSpace(model)
	}
}

func New(apiKey string, opts ...Option) *Parser {
	p := &Parser{
		apiKey: strings.TrimSpace(apiKey),
		model:  defaultModel,
		apiURL: defaultAPIURL,
		client: &http.Client{Timeout: 45 * time.Second},
	}
	for _, opt := range opts {
		opt(p)
	}
	if p.model == "" {
		p.model = defaultModel
	}
	if p.apiURL == "" {
		p.apiURL = defaultAPIURL
	}
	return p
}

func FromEnv(opts ...Option) (*Parser, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	}
	if apiKey == "" {
		return nil, errors.New("neither OPENROUTER_API_KEY nor OPENAI_API_KEY is configured for natural language preferences parser")
	}
	
	envOpts := []Option{}
	
	model := strings.TrimSpace(os.Getenv("OPENROUTER_PREFERENCES_MODEL"))
	if model == "" {
		model = strings.TrimSpace(os.Getenv("OPENAI_PREFERENCES_MODEL"))
	}
	if model != "" {
		envOpts = append(envOpts, WithModel(model))
	}
	
	baseURL := strings.TrimSpace(os.Getenv("OPENROUTER_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	}
	if baseURL != "" {
		envOpts = append(envOpts, WithAPIURL(strings.TrimRight(baseURL, "/")+"/chat/completions"))
	}
	
	envOpts = append(envOpts, opts...)
	return New(apiKey, envOpts...), nil
}

func (p *Parser) Normalize(ctx context.Context, input string) (string, error) {
	var out struct {
		NormalizedQuery string `json:"normalized_query"`
	}
	if err := p.call(ctx, normalizationInstructions, input, "normalized_preferences", normalizationSchema(), &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.NormalizedQuery), nil
}

func (p *Parser) Extract(ctx context.Context, normalizedInput string) (*domain.UserPreferences, error) {
	var out domain.UserPreferences
	if err := p.call(ctx, extractionInstructions, normalizedInput, "user_preferences", PreferencesSchema(), &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *Parser) call(ctx context.Context, instructions, input, schemaName string, schema map[string]any, out any) error {
	if p.apiKey == "" {
		return errors.New("missing API key")
	}
	payload := chatRequest{
		Model: p.model,
		Messages: []chatMessage{
			{
				Role:    "system",
				Content: instructions,
			},
			{
				Role:    "user",
				Content: input,
			},
		},
		ResponseFormat: responseFormat{
			Type: "json_schema",
			JSONSchema: jsonSchema{
				Name:   schemaName,
				Strict: true,
				Schema: schema,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("preferences API status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	outputText, err := extractOutputText(respBody)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(outputText), out)
}

const normalizationInstructions = `Ты выполняешь только нормализацию требований пользователя.
Твоя задача улучшить структуру текста и читаемость.
Нормализованное описание должно быть на том же языке, на котором пользователь написал запрос.
Если запрос написан на русском, весь normalized_query должен быть на русском; не используй английские слова, названия полей, служебные slugs или enum-значения.
Если в исходном запросе есть иностранное собственное название, сохрани его только если нет очевидного общеупотребимого варианта на языке пользователя.
Запрещено добавлять новые требования, предположения, районы, удобства, площади, цены, характеристики или предпочтения, которых нет в исходном тексте.
Если требование неоднозначно, сохрани его как есть.
Не меняй числовые значения.
Не заменяй "желательно" на "обязательно" и не заменяй "обязательно" на "желательно".
Верни только strict JSON по схеме.`

const extractionInstructions = `Extract apartment search preferences from the normalized user requirements.
Return only strict JSON matching the schema.
Use null for missing numeric values and empty arrays for missing lists.
Do not invent exact coordinates.
Coordinates and Google place IDs must remain empty unless present in the input.
Human-readable text fields must preserve the input language: name, center_name, boundary_street_name, raw_text, and unresolved_location_rules.
If the input is Russian, keep those human-readable text fields in Russian and do not translate them into English.
Use stable English slugs only for machine-readable enum/list fields such as required_amenities, preferred_amenities, interior_preferences.style, interior_preferences.reject, reject_rules, type, and boundary_rule.
Ambiguous location phrases must be preserved in unresolved_location_rules in the original input language.
CRITICAL: Only one location or city is allowed per user. If the input specifies multiple locations, choose the primary one and include only one object in the location_preferences array.`

func normalizationSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"normalized_query"},
		"properties": map[string]any{
			"normalized_query": map[string]any{"type": "string"},
		},
	}
}

func PreferencesSchema() map[string]any {
	locationPreference := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"name", "type", "center_name", "radius_meters", "boundary_street_name",
			"boundary_rule", "geometry_wkt", "google_place_id", "confidence", "raw_text", "unresolved_rules",
		},
		"properties": map[string]any{
			"name":                 map[string]any{"type": "string"},
			"type":                 map[string]any{"type": "string"},
			"center_name":          map[string]any{"type": "string"},
			"radius_meters":        nullableInteger(),
			"boundary_street_name": nullableString(),
			"boundary_rule":        nullableString(),
			"geometry_wkt":         nullableString(),
			"google_place_id":      nullableString(),
			"confidence":           map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			"raw_text":             map[string]any{"type": "string"},
			"unresolved_rules":     stringArray(),
		},
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"budget_min", "budget_max", "area_min", "area_max", "required_amenities",
			"preferred_amenities", "interior_preferences", "reject_rules",
			"location_preferences", "weights", "unresolved_location_rules",
		},
		"properties": map[string]any{
			"budget_min":          nullableInteger(),
			"budget_max":          nullableInteger(),
			"area_min":            nullableNumber(),
			"area_max":            nullableNumber(),
			"required_amenities":  stringArray(),
			"preferred_amenities": stringArray(),
			"interior_preferences": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"style", "reject"},
				"properties": map[string]any{
					"style":  stringArray(),
					"reject": stringArray(),
				},
			},
			"reject_rules": stringArray(),
			"location_preferences": map[string]any{
				"type":  "array",
				"items": locationPreference,
			},
			"weights": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"location", "interior", "view", "price", "amenities"},
				"properties": map[string]any{
					"location":  map[string]any{"type": "integer"},
					"interior":  map[string]any{"type": "integer"},
					"view":      map[string]any{"type": "integer"},
					"price":     map[string]any{"type": "integer"},
					"amenities": map[string]any{"type": "integer"},
				},
			},
			"unresolved_location_rules": stringArray(),
		},
	}
}

func nullableInteger() map[string]any {
	return map[string]any{"type": []string{"integer", "null"}}
}

func nullableNumber() map[string]any {
	return map[string]any{"type": []string{"number", "null"}}
}

func nullableString() map[string]any {
	return map[string]any{"type": []string{"string", "null"}}
}

func stringArray() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}

type chatRequest struct {
	Model          string         `json:"model"`
	Messages       []chatMessage  `json:"messages"`
	ResponseFormat responseFormat `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type responseFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

func extractOutputText(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", err
	}
	if len(response.Choices) == 0 {
		return "", errors.New("API response did not contain choices")
	}
	return response.Choices[0].Message.Content, nil
}
