package openaipreferences

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

func TestAnthropicParserSendsJSONSchemaAndParsesText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model        string `json:"model"`
			OutputConfig struct {
				Effort string `json:"effort"`
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
		if req.OutputConfig.Format.Type != "json_schema" || req.OutputConfig.Format.Schema["type"] != "object" {
			t.Fatalf("expected json_schema output format, got %+v", req.OutputConfig.Format)
		}
		if req.OutputConfig.Effort != "low" {
			t.Fatalf("expected effort low, got %q", req.OutputConfig.Effort)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"{\"normalized_query\":\"  Ваке  \"}"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	parser := NewAnthropic("test-key", "", "low", option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	got, err := parser.Normalize(context.Background(), "ваке")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Ваке" {
		t.Fatalf("unexpected normalized query %q", got)
	}
}

func TestAnthropicParserRejectsTruncatedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"max_tokens","content":[{"type":"text","text":"{\"normalized"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer server.Close()

	parser := NewAnthropic("test-key", "", "", option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	if _, err := parser.Normalize(context.Background(), "ваке"); err == nil {
		t.Fatal("expected error for max_tokens stop reason")
	}
}
