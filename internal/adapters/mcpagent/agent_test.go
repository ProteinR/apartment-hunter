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
	profile          profiles.Profile
	evaluationRecord domain.ListingEvaluationRecord
	evaluationFound  bool
	gotProfileID     string
	gotChatID        int64
	gotMessageID     int
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

func (s *fakeProfileStore) GetEvaluationByTelegramMessage(ctx context.Context, profileID string, telegramChatID int64, messageID int) (domain.ListingEvaluationRecord, bool, error) {
	s.gotProfileID = profileID
	s.gotChatID = telegramChatID
	s.gotMessageID = messageID
	return s.evaluationRecord, s.evaluationFound, nil
}

type fakeChatLister struct {
	limit     int
	accountID int64
}

func (l *fakeChatLister) AccountUserID(ctx context.Context) (int64, error) {
	return l.accountID, nil
}

func (l *fakeChatLister) ListAvailableChats(ctx context.Context, limit int) ([]sources.AvailableChat, error) {
	l.limit = limit
	return []sources.AvailableChat{
		{ID: "10", Title: "Batumi Rent", Username: "batumi_rent", Kind: "channel"},
		{ID: "20", Title: "Random News", Username: "random_news", Kind: "channel"},
	}, nil
}

func TestProcessCommandCanListAvailableTelegramChats(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42}}
	lister := &fakeChatLister{accountID: 42}
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

func TestProcessCommandIncludesConnectedSourcesGuidance(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{
		ID: "telegram_42",
		Channels: []profiles.ChannelSubscription{{
			Source: sources.ChannelRef{
				ID:       "batumi_rent",
				Username: "batumi_rent",
				Title:    "Batumi Rent",
			},
		}},
	}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		foundSource := false
		foundGuidance := false
		for _, msg := range req.Messages {
			if msg.Role != "system" {
				continue
			}
			if strings.Contains(msg.Content, "batumi_rent") && strings.Contains(msg.Content, "Batumi Rent") {
				foundSource = true
			}
			if strings.Contains(msg.Content, "отвечай строго по \"ConnectedSources\"") && strings.Contains(msg.Content, "НЕ говори, что ты не подключен к списку чатов") {
				foundGuidance = true
			}
		}
		if !foundSource {
			t.Fatalf("expected connected source in system prompt: %+v", req.Messages)
		}
		if !foundGuidance {
			t.Fatalf("expected connected sources guidance in system prompt: %+v", req.Messages)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Сейчас подключен @batumi_rent — Batumi Rent."}}]}`))
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL

	reply, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "В каких чатах сейчас ты читаешь?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "@batumi_rent") {
		t.Fatalf("unexpected reply %q", reply)
	}
}

func TestProcessCommandNormalizesUpdatedPreferences(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42"}}
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
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"update_preferences","arguments":"{\"budget_min\":400,\"budget_max\":800,\"area_min\":45,\"required_amenities\":[\"dishwasher\",\"oven\"],\"stop_words\":[\"продажа\",\"покупка\"]}"}}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Готово."}}]}`))
		default:
			t.Fatalf("unexpected request #%d", requests)
		}
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL

	_, prefsChanged, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "Добавь стоп слова о продаже либо покупке квартир")
	if err != nil {
		t.Fatal(err)
	}
	if prefsChanged == nil {
		t.Fatal("expected preferences to be changed")
	}
	if !prefsChanged.RequireDishwasher || !prefsChanged.RequireOven {
		t.Fatalf("expected legacy appliance flags to be normalized, got %+v", prefsChanged)
	}
	if prefsChanged.MinBudget != 400 || prefsChanged.MaxBudget != 800 || prefsChanged.MinAreaM2 != 45 {
		t.Fatalf("expected legacy budget/area fields to be normalized, got %+v", prefsChanged)
	}
	if store.profile.Preferences == nil || !store.profile.Preferences.RequireDishwasher || !store.profile.Preferences.RequireOven {
		t.Fatalf("expected stored preferences to keep appliance requirements, got %+v", store.profile.Preferences)
	}
}

func TestProcessCommandMergesPartialPreferenceUpdate(t *testing.T) {
	areaMin := 45.0
	budgetMin := 400
	budgetMax := 800
	store := &fakeProfileStore{profile: profiles.Profile{
		ID: "telegram_42",
		Preferences: &domain.UserPreferences{
			MinBudget: 400,
			MaxBudget: 800,
			MinAreaM2: 45,
			BudgetMin: &budgetMin,
			BudgetMax: &budgetMax,
			AreaMin:   &areaMin,
			InteriorPreferences: domain.InteriorPreferences{
				Style:  []string{"modern", "light_renovation"},
				Reject: []string{"glossy_ceiling"},
			},
			RejectRules: []string{"windows_facing_neighbor_building"},
			StopWords:   []string{"продажа", "покупка"},
			Weights:     domain.PreferenceWeights{Location: 5, Interior: 4, View: 2, Price: 3, Amenities: 5},
		},
	}}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		switch requests {
		case 1:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"update_preferences","arguments":"{\"required_amenities\":[\"dishwasher\",\"oven\"]}"}}]}}]}`))
		case 2:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Готово."}}]}`))
		default:
			t.Fatalf("unexpected request #%d", requests)
		}
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL

	_, prefsChanged, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "обнови")
	if err != nil {
		t.Fatal(err)
	}
	if prefsChanged == nil {
		t.Fatal("expected preferences to be changed")
	}
	if !prefsChanged.RequireDishwasher || !prefsChanged.RequireOven {
		t.Fatalf("expected appliance requirements to be enabled, got %+v", prefsChanged)
	}
	if prefsChanged.MinBudget != 400 || prefsChanged.MaxBudget != 800 || prefsChanged.MinAreaM2 != 45 {
		t.Fatalf("expected existing budget and area to be preserved, got %+v", prefsChanged)
	}
	if len(prefsChanged.InteriorPreferences.Style) != 2 || prefsChanged.InteriorPreferences.Style[0] != "modern" {
		t.Fatalf("expected existing interior preferences to be preserved, got %+v", prefsChanged.InteriorPreferences)
	}
	if len(prefsChanged.StopWords) != 2 || prefsChanged.StopWords[0] != "продажа" {
		t.Fatalf("expected existing stop words to be preserved, got %+v", prefsChanged.StopWords)
	}
	if store.profile.Preferences == nil || !store.profile.Preferences.RequireDishwasher || !store.profile.Preferences.RequireOven {
		t.Fatalf("expected stored preferences to include appliance requirements, got %+v", store.profile.Preferences)
	}
}

func TestProcessFeedbackCanGetListingEvaluationReport(t *testing.T) {
	store := &fakeProfileStore{
		profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42},
		evaluationRecord: domain.ListingEvaluationRecord{
			Listing: domain.ApartmentListing{
				ID:          "listing-1",
				Title:       "Flat near park",
				Description: "Nice flat",
				Price:       700,
			},
			Source: domain.ListingSourceRef{
				ProfileID:      "telegram_42",
				TelegramChatID: 42,
				MessageID:      321,
			},
			Status: "passed",
			Score: &domain.ListingScore{
				PassedHardFilters: true,
				ShortSummary:      "Flat near park passed hard filters.",
			},
			Report: "✅ Подходит",
		},
		evaluationFound: true,
	}
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
			hasReplyContext := false
			for _, tool := range req.Tools {
				if tool.Function.Name == "get_listing_evaluation_report" {
					hasTool = true
					break
				}
			}
			for _, msg := range req.Messages {
				if msg.Role == "user" && strings.Contains(msg.Content, "replied_message_id: 555") {
					hasReplyContext = true
					break
				}
			}
			if !hasTool {
				t.Fatalf("expected get_listing_evaluation_report tool in request: %+v", req.Tools)
			}
			if !hasReplyContext {
				t.Fatalf("expected replied message context in request: %+v", req.Messages)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"get_listing_evaluation_report","arguments":"{\"telegram_chat_id\":42,\"message_id\":555}"}}]}}]}`))
		case 2:
			foundToolResult := false
			for _, msg := range req.Messages {
				if msg.Role == "tool" && msg.Name == "get_listing_evaluation_report" && strings.Contains(msg.Content, `"status":"passed"`) && strings.Contains(msg.Content, "Подходит") {
					foundToolResult = true
					break
				}
			}
			if !foundToolResult {
				t.Fatalf("expected evaluation report tool result in follow-up request: %+v", req.Messages)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Оно прошло, потому что цена и базовые фильтры совпали."}}]}`))
		default:
			t.Fatalf("unexpected request #%d", requests)
		}
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL

	reply, _, _, err := agent.ProcessFeedback(context.Background(), "telegram_42", store.evaluationRecord.Listing, "почему прошло?", 42, 555)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Оно прошло, потому что цена и базовые фильтры совпали." {
		t.Fatalf("unexpected reply %q", reply)
	}
	if store.gotProfileID != "telegram_42" || store.gotChatID != 42 || store.gotMessageID != 555 {
		t.Fatalf("unexpected lookup args: profile=%s chat=%d message=%d", store.gotProfileID, store.gotChatID, store.gotMessageID)
	}
}

func TestMergePreferenceUpdateChangesCityOnlyWhenPatchNamesOne(t *testing.T) {
	existing := domain.UserPreferences{City: "tbilisi", MaxBudget: 800}

	kept, err := mergePreferenceUpdate(&existing, `{"city":null,"budget_max":900}`)
	if err != nil {
		t.Fatal(err)
	}
	if kept.City != "tbilisi" || kept.MaxBudget != 900 {
		t.Fatalf("expected Tbilisi kept with new budget, got %+v", kept)
	}

	switched, err := mergePreferenceUpdate(&existing, `{"city":"batumi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if switched.City != "batumi" {
		t.Fatalf("expected Batumi, got %q", switched.City)
	}

	ignored, err := mergePreferenceUpdate(&existing, `{"city":"kutaisi"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ignored.City != "tbilisi" {
		t.Fatalf("expected unsupported city to be ignored, got %q", ignored.City)
	}
}

func TestProcessCommandHidesUserbotChatsFromOtherUsers(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_7", TelegramChatID: 7}}
	lister := &fakeChatLister{accountID: 42}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		for _, tool := range req.Tools {
			if tool.Function.Name == "list_available_telegram_chats" {
				t.Fatalf("expected no userbot chat tool for another user")
			}
		}
		if strings.Contains(req.Messages[0].Content, "list_available_telegram_chats") {
			t.Fatalf("expected no userbot chat guidance for another user")
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Messages[len(req.Messages)-1].Role == "tool" {
			if req.Messages[len(req.Messages)-1].Content != "unknown function" {
				t.Fatalf("expected the hidden tool to be refused, got %q", req.Messages[len(req.Messages)-1].Content)
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Пришли @username канала."}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_available_telegram_chats","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL
	agent.SetTelegramChatLister(lister)

	if _, _, _, err := agent.ProcessCommand(context.Background(), "telegram_7", "Какие чаты можно отслеживать?"); err != nil {
		t.Fatal(err)
	}
	if lister.limit != 0 {
		t.Fatalf("expected userbot chats not to be listed, got limit %d", lister.limit)
	}
}

func TestProcessCommandChainsToolCallsAcrossRounds(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42, Preferences: &domain.UserPreferences{}}}
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
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"calculate_distance","arguments":"{\"lat1\":41.7,\"lon1\":44.7,\"lat2\":41.71,\"lon2\":44.7}"}}]}}]}`))
		case 2:
			if len(req.Tools) == 0 {
				t.Fatalf("expected tools to stay available for a second round")
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"update_preferences","arguments":"{\"budget_max\":700}"}}]}}]}`))
		default:
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Готово."}}]}`))
		}
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL

	reply, prefs, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "Поставь бюджет 700")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Готово." || prefs == nil || prefs.BudgetMax == nil || *prefs.BudgetMax != 700 || requests != 3 {
		t.Fatalf("expected chained update, got reply=%q prefs=%+v requests=%d", reply, prefs, requests)
	}
}

func TestProcessCommandStopsOfferingToolsAfterRoundLimit(t *testing.T) {
	store := &fakeProfileStore{profile: profiles.Profile{ID: "telegram_42", TelegramChatID: 42}}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if req.ToolChoice == "none" {
			if len(req.Tools) == 0 {
				t.Fatalf("expected tools to stay defined alongside tool history")
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Итог."}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call","type":"function","function":{"name":"calculate_distance","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()

	agent := New("test-key", store)
	agent.apiURL = server.URL

	reply, _, _, err := agent.ProcessCommand(context.Background(), "telegram_42", "Посчитай")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Итог." || requests != maxAgentToolRounds+1 {
		t.Fatalf("expected a final answer after %d tool rounds, got %q after %d requests", maxAgentToolRounds, reply, requests)
	}
}
