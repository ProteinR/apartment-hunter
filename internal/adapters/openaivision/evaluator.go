package openaivision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/domain"
)

const (
	defaultAPIURL      = "https://api.openai.com/v1/responses"
	defaultModel       = "gpt-4.1-mini"
	defaultMaxImageDim = 768
	defaultMaxPhotos   = 8
)

type Evaluator struct {
	apiKey      string
	model       string
	apiURL      string
	maxImageDim int
	maxPhotos   int
	pricing     Pricing
	client      *http.Client

	mu      sync.Mutex
	lastErr error
}

type Option func(*Evaluator)

type Pricing struct {
	InputPerMillionUSD       float64
	CachedInputPerMillionUSD float64
	OutputPerMillionUSD      float64
}

func WithHTTPClient(client *http.Client) Option {
	return func(e *Evaluator) {
		e.client = client
	}
}

func WithAPIURL(apiURL string) Option {
	return func(e *Evaluator) {
		e.apiURL = strings.TrimSpace(apiURL)
	}
}

func WithModel(model string) Option {
	return func(e *Evaluator) {
		e.model = strings.TrimSpace(model)
	}
}

func WithMaxImageDim(maxImageDim int) Option {
	return func(e *Evaluator) {
		e.maxImageDim = maxImageDim
	}
}

func WithMaxPhotos(maxPhotos int) Option {
	return func(e *Evaluator) {
		e.maxPhotos = maxPhotos
	}
}

func WithPricing(pricing Pricing) Option {
	return func(e *Evaluator) {
		e.pricing = pricing
	}
}

func New(apiKey string, opts ...Option) *Evaluator {
	e := &Evaluator{
		apiKey:      strings.TrimSpace(apiKey),
		model:       defaultModel,
		apiURL:      defaultAPIURL,
		maxImageDim: defaultMaxImageDim,
		maxPhotos:   defaultMaxPhotos,
		pricing:     pricingFromEnv(),
		client:      &http.Client{Timeout: 60 * time.Second},
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.model == "" {
		e.model = defaultModel
	}
	if e.apiURL == "" {
		e.apiURL = defaultAPIURL
	}
	if e.maxImageDim <= 0 {
		e.maxImageDim = defaultMaxImageDim
	}
	if e.maxPhotos <= 0 {
		e.maxPhotos = defaultMaxPhotos
	}
	return e
}

func FromEnv(opts ...Option) (*Evaluator, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required for OpenAI vision evaluator")
	}
	envOpts := []Option{}
	if model := strings.TrimSpace(os.Getenv("OPENAI_VISION_MODEL")); model != "" {
		envOpts = append(envOpts, WithModel(model))
	}
	if apiURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL")); apiURL != "" {
		envOpts = append(envOpts, WithAPIURL(strings.TrimRight(apiURL, "/")+"/responses"))
	}
	envOpts = append(envOpts, opts...)
	return New(apiKey, envOpts...), nil
}

func (e *Evaluator) Analyze(listing domain.ApartmentListing) domain.ListingInsights {
	insights, _, err := e.analyze(context.Background(), listing)
	e.setLastError(err)
	if err != nil {
		return domain.ListingInsights{
			Style:           "unknown",
			ViewQuality:     0.5,
			InteriorQuality: 0.5,
			DescriptionHighlights: []string{
				"openai vision unavailable: " + err.Error(),
			},
		}
	}
	return insights
}

func (e *Evaluator) AnalyzeWithUsage(ctx context.Context, listing domain.ApartmentListing) (domain.ListingInsights, domain.AIUsage, error) {
	insights, usage, err := e.analyze(ctx, listing)
	e.setLastError(err)
	return insights, usage, err
}

func (e *Evaluator) LastError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastErr
}

func (e *Evaluator) analyze(ctx context.Context, listing domain.ApartmentListing) (domain.ListingInsights, domain.AIUsage, error) {
	if e.apiKey == "" {
		return domain.ListingInsights{}, domain.AIUsage{}, errors.New("missing OpenAI API key")
	}

	content := []contentItem{{
		Type: "input_text",
		Text: listingPrompt(listing),
	}}

	for i, photo := range listing.Photos {
		if i >= e.maxPhotos {
			break
		}
		dataURL, err := e.resizedPhotoDataURL(ctx, photo.URL)
		if err != nil {
			continue
		}
		content = append(content, contentItem{
			Type:     "input_image",
			ImageURL: dataURL,
			Detail:   "low",
		})
	}

	payload := responsesRequest{
		Model:        e.model,
		Instructions: "You evaluate rental apartment listings. Use the listing text and photos. Return only structured JSON matching the schema. Be conservative: mark visual defects true only when clearly visible or directly stated. Mark dishwasher and oven true when they are directly stated in text or clearly visible in the kitchen photos.",
		Input: []inputMessage{{
			Role:    "user",
			Content: content,
		}},
		Text: textConfig{Format: jsonSchemaFormat()},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return domain.ListingInsights{}, domain.AIUsage{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.apiURL, bytes.NewReader(body))
	if err != nil {
		return domain.ListingInsights{}, domain.AIUsage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.client.Do(req)
	if err != nil {
		return domain.ListingInsights{}, domain.AIUsage{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return domain.ListingInsights{}, domain.AIUsage{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return domain.ListingInsights{}, domain.AIUsage{}, fmt.Errorf("openai API status %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	outputText, usage, err := extractOutputTextAndUsage(respBody, e.model, e.pricing)
	if err != nil {
		return domain.ListingInsights{}, usage, err
	}
	var insights domain.ListingInsights
	if err := json.Unmarshal([]byte(outputText), &insights); err != nil {
		return domain.ListingInsights{}, usage, err
	}
	return normalizeInsights(insights), usage, nil
}

func (e *Evaluator) resizedPhotoDataURL(ctx context.Context, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" || strings.HasPrefix(source, "mock://") {
		return "", errors.New("unsupported photo source")
	}

	raw, err := readPhoto(ctx, e.client, source)
	if err != nil {
		return "", err
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", err
	}

	resized := resizeToMaxDim(img, e.maxImageDim)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, resized, &jpeg.Options{Quality: 82}); err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

func (e *Evaluator) setLastError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastErr = err
}

func readPhoto(ctx context.Context, client *http.Client, source string) ([]byte, error) {
	parsed, err := url.Parse(source)
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("photo status %d for %s", resp.StatusCode, source)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	}

	if parsed != nil && parsed.Scheme == "file" {
		path, err := url.PathUnescape(parsed.Path)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	}
	return os.ReadFile(filepath.Clean(source))
}

func resizeToMaxDim(img image.Image, maxDim int) image.Image {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= maxDim && height <= maxDim {
		dst := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.Draw(dst, dst.Bounds(), img, bounds.Min, draw.Src)
		return dst
	}

	scale := float64(maxDim) / float64(max(width, height))
	newWidth := max(1, int(math.Round(float64(width)*scale)))
	newHeight := max(1, int(math.Round(float64(height)*scale)))
	dst := image.NewRGBA(image.Rect(0, 0, newWidth, newHeight))
	for y := 0; y < newHeight; y++ {
		srcY := bounds.Min.Y + min(height-1, int(float64(y)/scale))
		for x := 0; x < newWidth; x++ {
			srcX := bounds.Min.X + min(width-1, int(float64(x)/scale))
			dst.Set(x, y, img.At(srcX, srcY))
		}
	}
	return dst
}

func listingPrompt(listing domain.ApartmentListing) string {
	type photoMeta struct {
		URL     string `json:"url"`
		Room    string `json:"room"`
		Caption string `json:"caption"`
	}
	payload := struct {
		ID          string      `json:"id"`
		Title       string      `json:"title"`
		Description string      `json:"description"`
		District    string      `json:"district"`
		Price       int         `json:"price"`
		AreaM2      float64     `json:"area_m2"`
		Photos      []photoMeta `json:"photos"`
	}{
		ID:          listing.ID,
		Title:       listing.Title,
		Description: listing.Description,
		District:    listing.District,
		Price:       listing.Price,
		AreaM2:      listing.AreaM2,
	}
	for _, photo := range listing.Photos {
		payload.Photos = append(payload.Photos, photoMeta{
			URL:     photo.URL,
			Room:    photo.Room,
			Caption: photo.Caption,
		})
	}
	data, _ := json.Marshal(payload)
	return "Evaluate this apartment listing and attached resized photos. Scores must be floats from 0 to 1. Listing JSON: " + string(data)
}

func normalizeInsights(insights domain.ListingInsights) domain.ListingInsights {
	if strings.TrimSpace(insights.Style) == "" {
		insights.Style = "unknown"
	}
	insights.ViewQuality = clamp01(insights.ViewQuality)
	insights.InteriorQuality = clamp01(insights.InteriorQuality)
	if insights.DescriptionHighlights == nil {
		insights.DescriptionHighlights = []string{}
	}
	return insights
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func init() {
	image.RegisterFormat("png", "png", png.Decode, png.DecodeConfig)
}

type responsesRequest struct {
	Model        string         `json:"model"`
	Instructions string         `json:"instructions"`
	Input        []inputMessage `json:"input"`
	Text         textConfig     `json:"text"`
}

type inputMessage struct {
	Role    string        `json:"role"`
	Content []contentItem `json:"content"`
}

type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

type textConfig struct {
	Format jsonSchema `json:"format"`
}

type jsonSchema struct {
	Type   string         `json:"type"`
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

func jsonSchemaFormat() jsonSchema {
	return jsonSchema{
		Type:   "json_schema",
		Name:   "listing_insights",
		Strict: true,
		Schema: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required": []string{
				"style",
				"has_dishwasher",
				"has_oven",
				"has_glossy_ceilings",
				"has_dark_walls",
				"has_old_renovation",
				"windows_facing_house",
				"view_quality",
				"interior_quality",
				"description_highlights",
			},
			"properties": map[string]any{
				"style": map[string]any{
					"type":        "string",
					"description": "Interior style such as minimalist, scandinavian, loft, classic, modern, or unknown.",
				},
				"has_dishwasher":       map[string]any{"type": "boolean", "description": "True when a dishwasher is directly stated or clearly visible in the photos."},
				"has_oven":             map[string]any{"type": "boolean", "description": "True when an oven or built-in oven is directly stated or clearly visible in the photos."},
				"has_glossy_ceilings":  map[string]any{"type": "boolean"},
				"has_dark_walls":       map[string]any{"type": "boolean"},
				"has_old_renovation":   map[string]any{"type": "boolean"},
				"windows_facing_house": map[string]any{"type": "boolean"},
				"view_quality": map[string]any{
					"type":        "number",
					"minimum":     0,
					"maximum":     1,
					"description": "0 means bad/blocked view, 1 means excellent open/panoramic/sea view.",
				},
				"interior_quality": map[string]any{
					"type":        "number",
					"minimum":     0,
					"maximum":     1,
					"description": "0 means poor/dated interior, 1 means fresh high-quality renovation.",
				},
				"description_highlights": map[string]any{
					"type":  "array",
					"items": map[string]any{"type": "string"},
				},
			},
		},
	}
}

func extractOutputText(body []byte) (string, error) {
	outputText, _, err := extractOutputTextAndUsage(body, "", Pricing{})
	return outputText, err
}

func extractOutputTextAndUsage(body []byte, model string, pricing Pricing) (string, domain.AIUsage, error) {
	var response struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		OutputText string `json:"output_text"`
		Usage      struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", domain.AIUsage{}, err
	}
	usage := usageFromResponse(model, pricing, response.Usage.InputTokens, response.Usage.InputTokensDetails.CachedTokens, response.Usage.OutputTokens, response.Usage.OutputTokensDetails.ReasoningTokens, response.Usage.TotalTokens)
	if strings.TrimSpace(response.OutputText) != "" {
		return response.OutputText, usage, nil
	}
	for _, output := range response.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" && strings.TrimSpace(content.Text) != "" {
				return content.Text, usage, nil
			}
		}
	}
	return "", usage, errors.New("missing output_text in OpenAI response")
}

func usageFromResponse(model string, pricing Pricing, inputTokens, cachedInputTokens, outputTokens, reasoningTokens, totalTokens int) domain.AIUsage {
	billableInput := max(0, inputTokens-cachedInputTokens)
	cost := (float64(billableInput)*pricing.InputPerMillionUSD +
		float64(cachedInputTokens)*pricing.CachedInputPerMillionUSD +
		float64(outputTokens)*pricing.OutputPerMillionUSD) / 1_000_000
	return domain.AIUsage{
		Provider:                      "openai",
		Model:                         model,
		InputTokens:                   inputTokens,
		CachedInputTokens:             cachedInputTokens,
		OutputTokens:                  outputTokens,
		ReasoningTokens:               reasoningTokens,
		TotalTokens:                   totalTokens,
		InputPricePerMillionUSD:       pricing.InputPerMillionUSD,
		CachedInputPricePerMillionUSD: pricing.CachedInputPerMillionUSD,
		OutputPricePerMillionUSD:      pricing.OutputPerMillionUSD,
		CostUSD:                       math.Round(cost*1_000_000) / 1_000_000,
	}
}

func pricingFromEnv() Pricing {
	return Pricing{
		InputPerMillionUSD:       envFloat("OPENAI_VISION_INPUT_PRICE_PER_MILLION_USD", 0.40),
		CachedInputPerMillionUSD: envFloat("OPENAI_VISION_CACHED_INPUT_PRICE_PER_MILLION_USD", 0.10),
		OutputPerMillionUSD:      envFloat("OPENAI_VISION_OUTPUT_PRICE_PER_MILLION_USD", 1.60),
	}
}

func envFloat(name string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
