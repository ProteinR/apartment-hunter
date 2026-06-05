package openaivision

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
)

func TestAnalyzeSendsResizedPhotoAndParsesInsights(t *testing.T) {
	var photoBody bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 120, 80))
	for y := 0; y < 80; y++ {
		for x := 0; x < 120; x++ {
			img.Set(x, y, color.RGBA{R: 200, G: 220, B: 240, A: 255})
		}
	}
	if err := png.Encode(&photoBody, img); err != nil {
		t.Fatal(err)
	}

	var sawImage bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == "https://images.example/listing.png" {
			return responseBytes(http.StatusOK, "image/png", photoBody.Bytes()), nil
		}
		if r.URL.String() != "https://api.example/responses" {
			t.Fatalf("unexpected URL %s", r.URL.String())
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("unexpected auth header %q", got)
		}

		var request responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Text.Format.Type != "json_schema" {
			t.Fatalf("expected json schema format, got %q", request.Text.Format.Type)
		}
		if len(request.Input) != 1 {
			t.Fatalf("expected one input message, got %d", len(request.Input))
		}
		for _, item := range request.Input[0].Content {
			if item.Type != "input_image" {
				continue
			}
			sawImage = true
			const prefix = "data:image/jpeg;base64,"
			if !strings.HasPrefix(item.ImageURL, prefix) {
				t.Fatalf("expected jpeg data URL, got %q", item.ImageURL[:min(len(item.ImageURL), 24)])
			}
			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(item.ImageURL, prefix))
			if err != nil {
				t.Fatal(err)
			}
			cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if max(cfg.Width, cfg.Height) > 40 {
				t.Fatalf("expected resized image max dim <= 40, got %dx%d", cfg.Width, cfg.Height)
			}
		}

		return response(http.StatusOK, "application/json", `{
			"output": [{
				"type": "message",
				"content": [{
					"type": "output_text",
					"text": "{\"style\":\"modern\",\"has_glossy_ceilings\":false,\"has_dark_walls\":false,\"has_old_renovation\":false,\"windows_facing_house\":true,\"view_quality\":0.2,\"interior_quality\":0.9,\"description_highlights\":[\"bright kitchen\"]}"
				}]
			}]
		}`), nil
	})}

	evaluator := New("test-key", WithAPIURL("https://api.example/responses"), WithMaxImageDim(40), WithHTTPClient(client))
	insights := evaluator.Analyze(domain.ApartmentListing{
		ID:          "listing-1",
		Title:       "Modern flat",
		Description: "Fresh renovation",
		Photos: []domain.ApartmentPhoto{{
			URL:  "https://images.example/listing.png",
			Room: "living room",
		}},
	})

	if !sawImage {
		t.Fatal("expected image input to be sent")
	}
	if err := evaluator.LastError(); err != nil {
		t.Fatalf("unexpected last error: %v", err)
	}
	if insights.Style != "modern" || !insights.WindowsFacingHouse {
		t.Fatalf("unexpected insights: %+v", insights)
	}
	if insights.InteriorQuality != 0.9 || insights.ViewQuality != 0.2 {
		t.Fatalf("unexpected scores: %+v", insights)
	}
}

func TestAnalyzeReturnsFallbackOnOpenAIError(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusBadRequest, "text/plain", "bad request"), nil
	})}

	evaluator := New("test-key", WithAPIURL("https://api.example/responses"), WithHTTPClient(client))
	insights := evaluator.Analyze(domain.ApartmentListing{Title: "Flat"})

	if insights.Style != "unknown" {
		t.Fatalf("expected fallback style, got %q", insights.Style)
	}
	if evaluator.LastError() == nil {
		t.Fatal("expected last error")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func response(status int, contentType, body string) *http.Response {
	return responseBytes(status, contentType, []byte(body))
}

func responseBytes(status int, contentType string, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(bytes.NewReader(body)),
	}
}
