package mcpagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
)

func historyOfTurns(turns int, createdAt time.Time) []profiles.MCPMessage {
	var history []profiles.MCPMessage
	for i := 0; i < turns; i++ {
		history = append(history,
			profiles.MCPMessage{Role: "user", Content: fmt.Sprintf("вопрос %d", i), CreatedAt: createdAt},
			profiles.MCPMessage{Role: "assistant", Content: fmt.Sprintf("ответ %d", i), CreatedAt: createdAt},
		)
	}
	return history
}

func newAgentServer(t *testing.T, handle func(req chatRequest) string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(handle(req)))
	}))
	t.Cleanup(server.Close)
	return server
}

func assistantReply(content string) string {
	data, _ := json.Marshal(content)
	return `{"choices":[{"message":{"role":"assistant","content":` + string(data) + `}}]}`
}

func TestProcessCommandCompactsLongHistoryIntoSummary(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", MCPHistory: historyOfTurns(20, time.Now())}}
	var summaryRequest string
	server := newAgentServer(t, func(req chatRequest) string {
		if len(req.Tools) == 0 {
			summaryRequest = req.Messages[len(req.Messages)-1].Content
			return assistantReply("- Ищет двушку в Батуми до 600$")
		}
		return assistantReply("Понял.")
	})
	agent := New("test-key", store)
	agent.apiURL = server.URL

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "еще вопрос"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summaryRequest, "вопрос 0") || strings.Contains(summaryRequest, "еще вопрос") {
		t.Fatalf("expected only the oldest messages to be summarized, got %q", summaryRequest)
	}
	history := store.profile.MCPHistory
	if len(history) != historyKeptAfterCompaction+1 || history[0].Role != summaryHistoryRole || history[0].Content != "- Ищет двушку в Батуми до 600$" {
		t.Fatalf("expected a summary followed by %d recent messages, got %+v", historyKeptAfterCompaction, history)
	}
	if history[1].Role != "user" || history[len(history)-1].Content != "Понял." {
		t.Fatalf("expected recent turns to start with a user message and end with the reply, got %+v", history)
	}
}

func TestProcessCommandPutsSummaryIntoSystemPromptAndFoldsExpiredMessages(t *testing.T) {
	stale := historyOfTurns(2, time.Now().Add(-48*time.Hour))
	history := append([]profiles.MCPMessage{{Role: summaryHistoryRole, Content: "- Хочет Ваке", CreatedAt: time.Now()}}, stale...)
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", MCPHistory: history}}
	var systemPrompt string
	var commandMessages int
	server := newAgentServer(t, func(req chatRequest) string {
		if len(req.Tools) == 0 {
			if !strings.Contains(req.Messages[1].Content, "- Хочет Ваке") || !strings.Contains(req.Messages[1].Content, "вопрос 1") {
				t.Fatalf("expected the previous summary and expired turns in the summary request, got %q", req.Messages[1].Content)
			}
			return assistantReply("- Хочет Ваке\n- Спрашивал про бюджет")
		}
		systemPrompt = req.Messages[0].Content
		commandMessages = len(req.Messages)
		return assistantReply("Ок.")
	})
	agent := New("test-key", store)
	agent.apiURL = server.URL

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "привет"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(systemPrompt, "Краткое содержание более раннего разговора") || !strings.Contains(systemPrompt, "- Хочет Ваке") {
		t.Fatalf("expected the summary in the system prompt, got %q", systemPrompt)
	}
	if commandMessages != 2 {
		t.Fatalf("expected expired turns to be left out of the prompt, got %d messages", commandMessages)
	}
	saved := store.profile.MCPHistory
	if len(saved) != 3 || saved[0].Content != "- Хочет Ваке\n- Спрашивал про бюджет" {
		t.Fatalf("expected expired turns folded into the summary, got %+v", saved)
	}
}

func TestCompactHistoryKeepsMessagesWhenSummaryFails(t *testing.T) {
	server := newAgentServer(t, func(req chatRequest) string { return `{"choices":[]}` })
	agent := New("test-key", &fakeProfileStore{})
	agent.apiURL = server.URL

	history := historyOfTurns(25, time.Now())
	compacted := agent.compactHistory(context.Background(), history)
	if len(compacted) != len(history) {
		t.Fatalf("expected history to stay intact under the hard limit, got %d messages", len(compacted))
	}
	compacted = agent.compactHistory(context.Background(), historyOfTurns(50, time.Now()))
	if len(compacted) != historyHardLimit {
		t.Fatalf("expected history capped at %d messages, got %d", historyHardLimit, len(compacted))
	}
}

func TestProcessCommandSetsNotificationMode(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", Preferences: &domain.UserPreferences{MaxBudget: 600}}}
	requests := 0
	server := newAgentServer(t, func(req chatRequest) string {
		requests++
		if requests == 1 {
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"set_notification_mode","arguments":"{\"mode\":\"matches_only\"}"}}]}}]}`
		}
		if got := req.Messages[len(req.Messages)-1].Content; got != "success" {
			t.Fatalf("expected success tool result, got %q", got)
		}
		return assistantReply("Буду присылать только подходящие.")
	})
	agent := New("test-key", store)
	agent.apiURL = server.URL

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "присылай только подходящие"); err != nil {
		t.Fatal(err)
	}
	if store.profile.DebugMode == nil || *store.profile.DebugMode {
		t.Fatalf("expected debug mode off, got %+v", store.profile.DebugMode)
	}
}

type fakeSourceManager struct {
	added []string
}

func (m *fakeSourceManager) ListSources(ctx context.Context, telegramChatID int64) ([]string, error) {
	return []string{"- @batumi_rent"}, nil
}

func (m *fakeSourceManager) AddSources(ctx context.Context, telegramChatID int64, refs []string) (string, error) {
	m.added = append(m.added, refs...)
	return "Добавлен @" + strings.TrimPrefix(refs[0], "@"), nil
}

func (m *fakeSourceManager) RemoveSources(ctx context.Context, telegramChatID int64, refs []string) (string, error) {
	return "", nil
}

func TestProcessCommandUsesOnboardingPromptAndSourceToolsForNewUser(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42}}
	var systemPrompt string
	var toolNames []string
	server := newAgentServer(t, func(req chatRequest) string {
		systemPrompt = req.Messages[0].Content
		for _, tool := range req.Tools {
			toolNames = append(toolNames, tool.Function.Name)
		}
		return assistantReply("В каком городе ищем?")
	})
	agent := New("test-key", store)
	agent.apiURL = server.URL
	agent.SetSourceManager(&fakeSourceManager{})

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "привет"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(systemPrompt, "проводишь первую настройку") || !strings.Contains(systemPrompt, "НЕ вызывай update_preferences, пока пользователь не подтвердил сводку") {
		t.Fatalf("expected onboarding instructions, got %q", systemPrompt)
	}
	joined := strings.Join(toolNames, ",")
	for _, name := range []string{"list_sources", "add_sources", "remove_sources", "set_notification_mode"} {
		if !strings.Contains(joined, name) {
			t.Fatalf("expected tool %s, got %s", name, joined)
		}
	}
}

func TestProcessCommandWithoutChatIDHidesSourceTools(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", Preferences: &domain.UserPreferences{MaxBudget: 600}}}
	server := newAgentServer(t, func(req chatRequest) string {
		for _, tool := range req.Tools {
			if tool.Function.Name == "add_sources" {
				t.Fatal("expected source tools to be hidden without a Telegram chat id")
			}
		}
		if !strings.Contains(req.Messages[0].Content, "Профиль поиска пользователя уже сохранен") {
			t.Fatalf("expected configured-profile instructions, got %q", req.Messages[0].Content)
		}
		return assistantReply("Ок.")
	})
	agent := New("test-key", store)
	agent.apiURL = server.URL
	agent.SetSourceManager(&fakeSourceManager{})

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "что у меня?"); err != nil {
		t.Fatal(err)
	}
}

func TestOnboardingHidesAndRejectsSearchAreaUpdates(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42}}
	requests := 0
	var toolResult string
	server := newAgentServer(t, func(req chatRequest) string {
		requests++
		if requests == 1 {
			for _, tool := range req.Tools {
				if tool.Function.Name == "update_search_area" || tool.Function.Name == "" {
					t.Fatalf("expected update_search_area to be hidden during onboarding, got %q", tool.Function.Name)
				}
			}
			return `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"update_search_area","arguments":"{\"geojson\":\"{}\"}"}}]}}]}`
		}
		toolResult = req.Messages[len(req.Messages)-1].Content
		return assistantReply("Сначала подтвердим профиль.")
	})
	agent := New("test-key", store)
	agent.apiURL = server.URL

	_, prefs, area, err := agent.ProcessCommand(context.Background(), "telegram_42", "вот моя зона")
	if err != nil {
		t.Fatal(err)
	}
	if prefs != nil || area != nil || !strings.HasPrefix(toolResult, "error: confirm and save") {
		t.Fatalf("expected the area update to be refused, got prefs=%+v area=%+v result=%q", prefs, area, toolResult)
	}
}

func TestProcessCommandSavesOnlyHistoryAtTheEnd(t *testing.T) {
	store := &recordingProfileStore{fakeProfileStore: fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", Preferences: &domain.UserPreferences{MaxBudget: 600}}}}
	server := newAgentServer(t, func(req chatRequest) string { return assistantReply("Ок.") })
	agent := New("test-key", store)
	agent.apiURL = server.URL

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "привет"); err != nil {
		t.Fatal(err)
	}
	last := store.upserts[len(store.upserts)-1]
	if last.DebugMode != nil || last.FilterDisabled != nil || last.Preferences != nil || len(last.MCPHistory) != 2 {
		t.Fatalf("expected the final save to carry only history, got %+v", last)
	}
}

type recordingProfileStore struct {
	fakeProfileStore
	upserts []profiles.Profile
}

func (s *recordingProfileStore) UpsertProfile(ctx context.Context, profile profiles.Profile) error {
	s.upserts = append(s.upserts, profile)
	return s.fakeProfileStore.UpsertProfile(ctx, profile)
}
