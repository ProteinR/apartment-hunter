package openaivision

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"apartment-hunter/internal/domain"
)

func TestAnthropicAnalyzeSendsPhotoWithSchemaAndReportsUsage(t *testing.T) {
	t.Setenv("ANTHROPIC_VISION_INPUT_PRICE_PER_MILLION_USD", "")
	t.Setenv("ANTHROPIC_VISION_CACHED_INPUT_PRICE_PER_MILLION_USD", "")
	t.Setenv("ANTHROPIC_VISION_OUTPUT_PRICE_PER_MILLION_USD", "")
	var photoBody bytes.Buffer
	if err := png.Encode(&photoBody, solidImage(120, 80)); err != nil {
		t.Fatal(err)
	}
	photoPath := filepath.Join(t.TempDir(), "listing.png")
	if err := os.WriteFile(photoPath, photoBody.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content []struct {
					Type   string `json:"type"`
					Source struct {
						Type      string `json:"type"`
						MediaType string `json:"media_type"`
						Data      string `json:"data"`
					} `json:"source"`
				} `json:"content"`
			} `json:"messages"`
			OutputConfig struct {
				Format struct {
					Type   string         `json:"type"`
					Schema map[string]any `json:"schema"`
				} `json:"format"`
			} `json:"output_config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != DefaultAnthropicModel {
			t.Fatalf("unexpected model %q", req.Model)
		}
		if req.OutputConfig.Format.Type != "json_schema" {
			t.Fatalf("expected json_schema output format, got %q", req.OutputConfig.Format.Type)
		}
		viewQuality := req.OutputConfig.Format.Schema["properties"].(map[string]any)["view_quality"].(map[string]any)
		if _, ok := viewQuality["minimum"]; ok {
			t.Fatalf("expected numeric constraints to be stripped, got %+v", viewQuality)
		}
		var images int
		for _, block := range req.Messages[0].Content {
			if block.Type == "image" {
				images++
				if block.Source.Type != "base64" || block.Source.MediaType != "image/jpeg" || block.Source.Data == "" {
					t.Fatalf("unexpected image block %+v", block)
				}
			}
		}
		if images != 1 {
			t.Fatalf("expected one image block, got %d", images)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"{\"style\":\"modern\",\"has_dishwasher\":true,\"has_oven\":false,\"has_glossy_ceilings\":false,\"has_dark_walls\":false,\"has_old_renovation\":false,\"windows_facing_house\":false,\"view_quality\":0.7,\"interior_quality\":0.8,\"description_highlights\":[]}"}],"usage":{"input_tokens":1000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":200}}`))
	}))
	defer server.Close()

	evaluator := NewAnthropic("test-key", "", "", option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	insights, usage, err := evaluator.AnalyzeWithUsage(context.Background(), domain.ApartmentListing{
		Title:  "Apartment",
		Photos: []domain.ApartmentPhoto{{URL: photoPath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if insights.Style != "modern" || !insights.HasDishwasher || insights.InteriorQuality != 0.8 {
		t.Fatalf("unexpected insights %+v", insights)
	}
	if usage.Provider != "anthropic" || usage.Model != DefaultAnthropicModel || usage.TotalTokens != 1200 {
		t.Fatalf("unexpected usage %+v", usage)
	}
	if usage.CostUSD != 0.0002 {
		t.Fatalf("expected Haiku cost 0.0002, got %v", usage.CostUSD)
	}
}

func TestAnthropicAnalyzeRejectsTruncatedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"max_tokens","content":[{"type":"text","text":"{\"style"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	evaluator := NewAnthropic("test-key", "", "", option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	if _, _, err := evaluator.AnalyzeWithUsage(context.Background(), domain.ApartmentListing{Title: "Apartment"}); err == nil {
		t.Fatal("expected error for max_tokens stop reason")
	}
}

func solidImage(width, height int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 220, B: 240, A: 255})
		}
	}
	return img
}
