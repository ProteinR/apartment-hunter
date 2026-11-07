package mcpagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"apartment-hunter/internal/profiles"
)

type anthropicTestRequest struct {
	Model  string `json:"model"`
	System []struct {
		Text string `json:"text"`
	} `json:"system"`
	Messages []struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
	ToolChoice *struct {
		Type string `json:"type"`
	} `json:"tool_choice"`
}

func TestAnthropicAgentRunsToolRoundTrip(t *testing.T) {
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_EFFORT", "")
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42}}
	lister := &fakeChatLister{accountID: 42}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "test-key" {
			t.Fatalf("missing api key header")
		}
		var req anthropicTestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req.Model != "claude-haiku-5-5" {
			t.Fatalf("unexpected model %q", req.Model)
		}
		if len(req.System) == 0 || req.System[0].Text == "" {
			t.Fatalf("expected system prompt, got %+v", req.System)
		}
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			found := false
			for _, tool := range req.Tools {
				if tool.Name == "list_available_telegram_chats" {
					found = tool.InputSchema["type"] == "object"
				}
			}
			if !found {
				t.Fatalf("expected list_available_telegram_chats object tool, got %+v", req.Tools)
			}
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"tool_use","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"tool_use","id":"toolu_1","name":"list_available_telegram_chats","input":{"limit":25}}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		case 2:
			last := req.Messages[len(req.Messages)-1]
			previous := req.Messages[len(req.Messages)-2]
			if previous.Role != "assistant" || len(previous.Content) != 2 {
				t.Fatalf("expected assistant turn with thinking and tool_use blocks, got %+v", previous)
			}
			var thinking struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			}
			_ = json.Unmarshal(previous.Content[0], &thinking)
			if thinking.Type != "thinking" || thinking.Signature != "sig" {
				t.Fatalf("expected thinking block passed back unchanged, got %s", previous.Content[0])
			}
			var result struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			}
			_ = json.Unmarshal(last.Content[0], &result)
			if last.Role != "user" || result.Type != "tool_result" || result.ToolUseID != "toolu_1" {
				t.Fatalf("expected tool_result for toolu_1, got %s", last.Content[0])
			}
			_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5","stop_reason":"end_turn","content":[{"type":"text","text":"Можно начать с @batumi_rent."}],"usage":{"input_tokens":1,"output_tokens":1}}`))
		default:
			t.Fatalf("unexpected request #%d", requests)
		}
	}))
	defer server.Close()

	agent := NewAnthropic("test-key", store, option.WithBaseURL(server.URL), option.WithMaxRetries(0))
	agent.SetTelegramChatLister(lister)

	reply, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "Какие чаты можно отслеживать?")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Можно начать с @batumi_rent." {
		t.Fatalf("unexpected reply %q", reply)
	}
	if lister.limit != 25 {
		t.Fatalf("expected lister limit 25, got %d", lister.limit)
	}
}

func TestAnthropicConversationGroupsToolResultsAndMapsToolChoiceNone(t *testing.T) {
	system, messages := anthropicConversation([]chatMessage{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: ""},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "", ToolCalls: []toolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: "1"},
		{Role: "tool", ToolCallID: "b", Content: "2"},
	})
	if len(system) != 1 || system[0].Text != "rules" {
		t.Fatalf("unexpected system %+v", system)
	}
	if len(messages) != 2 {
		t.Fatalf("expected user message and one grouped tool result message, got %d", len(messages))
	}
	if len(messages[1].Content) != 2 {
		t.Fatalf("expected both tool results in one user message, got %d blocks", len(messages[1].Content))
	}
}
