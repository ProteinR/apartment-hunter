package mcpagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"
)

type fakeProfileStore struct {
	profile profiles.Profile
}

func (s *fakeProfileStore) GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error) {
	if s.profile.ID != id {
		return profiles.Profile{}, false, nil
	}
	return s.profile, true, nil
}

func (s *fakeProfileStore) UpsertProfile(ctx context.Context, profile profiles.Profile) error {
	s.profile = profile
	return nil
}

func (s *fakeProfileStore) SearchEvaluations(ctx context.Context, profileID string, filter domain.EvaluationSearchFilter) ([]domain.ListingEvaluationRecord, error) {
	return nil, nil
}

type fakeChatLister struct {
	limit int
}

func (l *fakeChatLister) ListAvailableChats(ctx context.Context, limit int) ([]sources.AvailableChat, error) {
	l.limit = limit
	return []sources.AvailableChat{
		{ID: "10", Title: "Batumi Rent", Username: "batumi_rent", Kind: "channel"},
		{ID: "20", Title: "Random News", Username: "random_news", Kind: "channel"},
	}, nil
}

func TestProcessCommandCanListAvailableTelegramChats(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42"}}
	lister := &fakeChatLister{}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			hasTool := false
			for _, tool := range req.Tools {
				if tool.Function.Name == "list_available_telegram_chats" {
					hasTool = true
					break
				}
			}
			if !hasTool {
				t.Fatalf("expected list_available_telegram_chats tool in request: %+v", req.Tools)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_available_telegram_chats","arguments":"{\"limit\":25}"}}]}}]}`))
		case 2:
			foundToolResult := false
			for _, msg := range req.Messages {
				if msg.Role == "tool" && msg.Name == "list_available_telegram_chats" && strings.Contains(msg.Content, "batumi_rent") {
					foundToolResult = true
					break
				}
			}
			if !foundToolResult {
				t.Fatalf("expected Telegram chat tool result in follow-up request: %+v", req.Messages)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Можно начать с @batumi_rent."}}]}`))
		default:
			t.Fatalf("unexpected request #%d", requests)
		}
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL
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
