package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/adapters/mcpagent"
	"apartment-hunter/internal/cities"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	"github.com/go-telegram/bot/models"
)

const testSearchAreaGeoJSON = `{"type":"MultiPolygon","coordinates":[[[[41.620000,41.640000],[41.630000,41.640000],[41.630000,41.650000],[41.620000,41.650000],[41.620000,41.640000]]]]}`

type fakeProfileStore struct {
	profiles        map[string]profiles.Profile
	searchProfiles  []domain.UserSearchProfile
	latestGeoJSON   string
	feedbacks       []domain.ListingFeedback
	feedbackMatched bool
}

func newFakeProfileStore() *fakeProfileStore {
	return &fakeProfileStore{profiles: map[string]profiles.Profile{}}
}

func (s *fakeProfileStore) UpsertProfile(ctx context.Context, profile profiles.Profile) error {
	existing := s.profiles[profile.ID]
	if profile.TelegramChatID != 0 {
		existing.TelegramChatID = profile.TelegramChatID
	}
	if profile.Preferences != nil {
		prefs := *profile.Preferences
		existing.Preferences = &prefs
	}
	if profile.DebugMode != nil {
		debugMode := *profile.DebugMode
		existing.DebugMode = &debugMode
	}
	if profile.MCPMode != nil {
		mcpMode := *profile.MCPMode
		existing.MCPMode = &mcpMode
	}
	if profile.FilterDisabled != nil {
		filterDisabled := *profile.FilterDisabled
		existing.FilterDisabled = &filterDisabled
	}
	if profile.MCPHistory != nil {
		existing.MCPHistory = profile.MCPHistory
	}
	if profile.Channels != nil {
		existing.Channels = profile.Channels
	}
	existing.ID = profile.ID
	s.profiles[profile.ID] = existing
	return nil
}

func (s *fakeProfileStore) SaveSearchProfile(ctx context.Context, profile domain.UserSearchProfile) error {
	s.searchProfiles = append(s.searchProfiles, profile)
	return nil
}

func (s *fakeProfileStore) GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error) {
	profile, ok := s.profiles[id]
	return profile, ok, nil
}

func (s *fakeProfileStore) UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error) {
	profile := s.profiles[profileID]
	profile.ID = profileID
	for i := range profile.Channels {
		if profile.Channels[i].Source.ID == sub.Source.ID {
			profile.Channels[i] = sub
			s.profiles[profileID] = profile
			return false, nil
		}
	}
	profile.Channels = append(profile.Channels, sub)
	s.profiles[profileID] = profile
	return true, nil
}

func (s *fakeProfileStore) RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error) {
	profile := s.profiles[profileID]
	for i, sub := range profile.Channels {
		if sub.Source.ID == channel || sub.Source.Username == channel {
			profile.Channels = append(profile.Channels[:i], profile.Channels[i+1:]...)
			s.profiles[profileID] = profile
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeProfileStore) SaveListingFeedback(ctx context.Context, feedback domain.ListingFeedback) (string, *domain.ApartmentListing, error) {
	if !s.feedbackMatched {
		return "", nil, nil
	}
	feedback.ID = "7"
	s.feedbacks = append(s.feedbacks, feedback)
	return feedback.ID, &domain.ApartmentListing{ID: "dummy-listing"}, nil
}

func (s *fakeProfileStore) ListingFeedbackExists(ctx context.Context, telegramChatID int64, feedbackMessageID int) (bool, error) {
	for _, feedback := range s.feedbacks {
		if feedback.TelegramChatID == telegramChatID && feedback.FeedbackMessageID == feedbackMessageID {
			return true, nil
		}
	}
	return false, nil
}

func (s *fakeProfileStore) LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error) {
	if strings.TrimSpace(s.latestGeoJSON) == "" {
		return "", false, nil
	}
	return s.latestGeoJSON, true, nil
}

func (s *fakeProfileStore) SearchEvaluations(ctx context.Context, profileID string, filter domain.EvaluationSearchFilter) ([]domain.ListingEvaluationRecord, error) {
	return nil, nil
}

func (s *fakeProfileStore) GetEvaluationByTelegramMessage(ctx context.Context, profileID string, telegramChatID int64, messageID int) (domain.ListingEvaluationRecord, bool, error) {
	return domain.ListingEvaluationRecord{}, false, nil
}

func TestHandleTextDetectsTbilisiFromQueryAndBuildsTbilisiGeometry(t *testing.T) {
	store := newFakeProfileStore()
	var builtCities []string
	parser := preferenceparser.Service{
		Normalizer: fakeNormalizer{normalized: "Тбилиси, Ваке, до 900 USD"},
		Extractor: fakeExtractor{prefs: domain.UserPreferences{
			BudgetMax: intPtr(900),
			LocationPreferences: []domain.LocationPreference{{
				CenterName:   "Vake Park",
				RadiusMeters: intPtr(1500),
				Type:         "radius",
			}},
		}},
	}
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithPreferencesParser(parser), WithGeometryBuilder(fakeGeometryBuilder{cities: &builtCities}))
	chatID := int64(77)

	if _, err := bot.HandleText(chatID, "/preferences"); err != nil {
		t.Fatal(err)
	}
	reply, err := bot.HandleText(chatID, "Ищу квартиру в Тбилиси около парка Ваке, до 900$")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Город: Тбилиси") || !strings.Contains(reply, cities.Tbilisi.GeoJSONEditorURL()) {
		t.Fatalf("expected Tbilisi city and editor link in preview, got %q", reply)
	}
	if _, err := bot.HandleText(chatID, "Save"); err != nil {
		t.Fatal(err)
	}
	profile := store.profiles[profiles.IDForTelegramChat(chatID)]
	if profile.Preferences == nil || profile.Preferences.City != cities.Tbilisi.Key {
		t.Fatalf("expected saved Tbilisi city, got %+v", profile.Preferences)
	}
	if len(builtCities) == 0 || builtCities[0] != cities.Tbilisi.Key {
		t.Fatalf("expected geometry built for Tbilisi, got %+v", builtCities)
	}
}

func TestCityCommandSwitchesCityAndClearsPreviousCityLocations(t *testing.T) {
	store := newFakeProfileStore()
	chatID := int64(78)
	profileID := profiles.IDForTelegramChat(chatID)
	store.profiles[profileID] = profiles.Profile{ID: profileID, TelegramChatID: chatID, Preferences: &domain.UserPreferences{
		MaxBudget: 700,
		LocationPreferences: []domain.LocationPreference{{
			Name:         "6 May Park",
			Latitude:     41.6480,
			Longitude:    41.6285,
			RadiusMeters: intPtr(1000),
		}},
		HasSearchArea: true,
	}}
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON}))

	reply, err := bot.HandleText(chatID, "/city")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Город поиска: Батуми") {
		t.Fatalf("expected default Batumi, got %q", reply)
	}
	reply, err = bot.HandleText(chatID, "/city тбилиси")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Город поиска: Тбилиси") {
		t.Fatalf("unexpected reply %q", reply)
	}
	saved := store.profiles[profileID].Preferences
	if saved.City != cities.Tbilisi.Key || len(saved.LocationPreferences) != 0 || saved.HasSearchArea {
		t.Fatalf("expected Tbilisi without Batumi locations, got %+v", saved)
	}
	if len(store.searchProfiles) != 1 || store.searchProfiles[0].SearchArea == nil || store.searchProfiles[0].SearchArea.WKT != "" {
		t.Fatalf("expected one search profile with an explicitly empty area, got %+v", store.searchProfiles)
	}
}

func TestHandleTextParsesPreviewsAndSavesPreferences(t *testing.T) {
	outputDir := t.TempDir()
	store := newFakeProfileStore()
	parser := preferenceparser.Service{
		Normalizer: fakeNormalizer{normalized: "Бюджет:\n* до 650 USD"},
		Extractor: fakeExtractor{prefs: domain.UserPreferences{
			BudgetMax:          intPtr(650),
			AreaMin:            floatPtr(50),
			RequiredAmenities:  []string{"dishwasher", "oven"},
			PreferredAmenities: []string{"dishwasher", "oven"},
			InteriorPreferences: domain.InteriorPreferences{
				Style:  []string{"fresh", "light_renovation", "neutral_tones"},
				Reject: []string{"glossy_ceiling"},
			},
			RejectRules: []string{"window_facing_adjacent_building", "window_facing_construction_site"},
			LocationPreferences: []domain.LocationPreference{{
				CenterName:         "6 May Park",
				RadiusMeters:       intPtr(1000),
				BoundaryStreetName: stringPtr("Pushkin Street"),
				BoundaryRule:       stringPtr("not beyond"),
				RawText:            "район парка 6 мая",
				UnresolvedRules:    []string{},
				Confidence:         0.8,
				Type:               "radius_with_boundary",
			}},
			UnresolvedLocationRules: []string{"not beyond Pushkin Street"},
		}},
	}
	bot := New("test-token", outputDir, WithProfileStore(store), WithPreferencesParser(parser), WithGeometryBuilder(fakeGeometryBuilder{}))
	chatID := int64(42)

	reply, err := bot.HandleText(chatID, "/preferences")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Опиши, какую квартиру ищем") || !strings.Contains(reply, "Например:") {
		t.Fatalf("unexpected start reply %q", reply)
	}

	reply, err = bot.HandleText(chatID, "Ищу квартиру до 650 с посудомойкой")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Ваш запрос:") || !strings.Contains(reply, "Нормализованное описание:") || !strings.Contains(reply, "Бюджет: до 650 $") {
		t.Fatalf("unexpected preview:\n%s", reply)
	}
	for _, expected := range []string{
		"* посудомойка",
		"* духовка",
		"* свежий ремонт",
		"* светлый ремонт",
		"* глянцевые потолки",
		"* окна вплотную к соседнему дому",
		"* окна на строящийся объект",
		"* район парка 6 мая, радиус 1000 м",
		"* ограничение: не дальше улицы Пушкина",
		"Неразобранные гео-правила:",
		"* не дальше улицы Пушкина",
	} {
		if !strings.Contains(reply, expected) {
			t.Fatalf("expected preview to contain %q:\n%s", expected, reply)
		}
	}
	for _, unexpected := range []string{"* dishwasher", "* oven", "fresh_renovation", "light_renovation", "glossy_ceiling", "window_facing", "construction_site", "USD", "Pushkin Street", "Предпочтительно:"} {
		if strings.Contains(reply, unexpected) {
			t.Fatalf("preview should not expose %q:\n%s", unexpected, reply)
		}
	}
	if len(store.searchProfiles) != 0 {
		t.Fatal("preferences should not be saved before confirmation")
	}
	reply, err = bot.HandleText(chatID, "Сохранить")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Готово, фильтры сохранены.") ||
		!strings.Contains(reply, "Теперь нужны источники объявлений") ||
		!strings.Contains(reply, "не вступаю в чаты автоматически") {
		t.Fatalf("unexpected save reply %q", reply)
	}

	profile, ok, err := store.GetProfile(context.Background(), "telegram_42")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected telegram profile")
	}
	if profile.Preferences == nil {
		t.Fatal("expected preferences to be saved")
	}
	if profile.TelegramChatID != 42 {
		t.Fatalf("unexpected chat id %d", profile.TelegramChatID)
	}
	if profile.Preferences.MaxBudget != 650 {
		t.Fatalf("unexpected max budget %d", profile.Preferences.MaxBudget)
	}
	if len(store.searchProfiles) != 1 {
		t.Fatalf("expected search profile to be saved, got %d", len(store.searchProfiles))
	}
	if store.searchProfiles[0].RawQuery != "Ищу квартиру до 650 с посудомойкой" {
		t.Fatalf("unexpected raw query %q", store.searchProfiles[0].RawQuery)
	}
	if store.searchProfiles[0].NormalizedQuery != "Бюджет:\n* до 650 USD" {
		t.Fatalf("unexpected normalized query %q", store.searchProfiles[0].NormalizedQuery)
	}
}

func TestHandleTextResponseIncludesSearchAreaMapInConfirmation(t *testing.T) {
	store := newFakeProfileStore()
	parser := preferenceparser.Service{
		Normalizer: fakeNormalizer{normalized: "Ищу квартиру около парка 6 мая"},
		Extractor: fakeExtractor{prefs: domain.UserPreferences{
			BudgetMax: intPtr(650),
			LocationPreferences: []domain.LocationPreference{{
				CenterName:   "6 May Park",
				RadiusMeters: intPtr(1000),
				RawText:      "около парка 6 мая",
			}},
		}},
	}
	mapClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("markers") != "" {
			t.Fatalf("confirmation map should not include listing marker: %q", req.URL.RawQuery)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("png-bytes")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}

	bot := New(
		"test-token",
		t.TempDir(),
		WithProfileStore(store),
		WithPreferencesParser(parser),
		WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON}),
		WithSearchAreaMapRenderer(googlemaps.StaticMapRenderer{APIKey: "test-key", Endpoint: "https://static-maps.test/map", Client: mapClient}),
	)

	if _, err := bot.handleTextResponse(42, "/preferences"); err != nil {
		t.Fatal(err)
	}
	response, err := bot.handleTextResponse(42, "Ищу квартиру до 650 около парка 6 мая")
	if err != nil {
		t.Fatal(err)
	}
	if string(response.PhotoBytes) != "png-bytes" || response.PhotoFilename != "search-area-map.png" {
		t.Fatalf("expected map photo in response, got filename=%q bytes=%q", response.PhotoFilename, string(response.PhotoBytes))
	}
	if !strings.Contains(response.Text, "Распознанные фильтры:") || !strings.Contains(response.Text, "Сохранить?") {
		t.Fatalf("unexpected caption %q", response.Text)
	}
	if len([]rune(response.Text)) > 1024 {
		t.Fatalf("caption exceeds Telegram limit: %d", len([]rune(response.Text)))
	}
	if _, ok := response.ReplyMarkup.(*models.ReplyKeyboardMarkup); !ok {
		t.Fatalf("expected confirmation keyboard, got %T", response.ReplyMarkup)
	}
}

func TestMCPMapRequestAttachesSearchAreaImage(t *testing.T) {
	mcpMode := true
	store := newFakeProfileStore()
	store.latestGeoJSON = testSearchAreaGeoJSON
	store.profiles["telegram_42"] = profiles.Profile{
		ID:             "telegram_42",
		TelegramChatID: 42,
		MCPMode:        &mcpMode,
	}
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Прикрепляю изображение текущей зоны поиска."}}]}`))
	}))
	defer agentServer.Close()
	t.Setenv("OPENAI_BASE_URL", agentServer.URL)

	agent := mcpagent.New("test-key", store)
	mapClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Query().Get("path") == "" {
			t.Fatalf("expected search area path in static map URL: %q", req.URL.RawQuery)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("png-bytes")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	})}
	bot := New(
		"test-token",
		t.TempDir(),
		WithProfileStore(store),
		WithMCPAgent(agent),
		WithSearchAreaMapRenderer(googlemaps.StaticMapRenderer{APIKey: "test-key", Endpoint: "https://static-maps.test/map", Client: mapClient}),
	)

	response, err := bot.handleTextResponse(42, "Пришли мне изображение зоны поиска")
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != "Прикрепляю изображение текущей зоны поиска.\n\n"+searchAreaMapHint {
		t.Fatalf("unexpected MCP reply %q", response.Text)
	}
	if response.PhotoFilename != "search-area-map.png" || string(response.PhotoBytes) != "png-bytes" {
		t.Fatalf("expected map photo attachment, got filename=%q bytes=%q", response.PhotoFilename, string(response.PhotoBytes))
	}
}

func newScriptedAgentServer(t *testing.T, toolArguments string) *httptest.Server {
	t.Helper()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		requests++
		if requests == 1 {
			arguments, _ := json.Marshal(toolArguments)
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"update_preferences","arguments":` + string(arguments) + `}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Готово."}}]}`))
	}))
	t.Cleanup(server.Close)
	t.Setenv("OPENAI_BASE_URL", server.URL)
	return server
}

func batumiMCPProfile(chatID int64) profiles.Profile {
	mcpMode := true
	return profiles.Profile{
		ID:             profiles.IDForTelegramChat(chatID),
		TelegramChatID: chatID,
		MCPMode:        &mcpMode,
		Preferences: &domain.UserPreferences{
			MaxBudget: 600,
			LocationPreferences: []domain.LocationPreference{{
				Name:         "Парк 6 мая",
				CenterName:   "6 May Park",
				RadiusMeters: intPtr(1000),
			}},
			HasSearchArea: true,
		},
	}
}

func TestMCPMessageMentioningAnotherCityKeepsStoredCity(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(43)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"budget_max":900,"city":null}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	if _, err := bot.handleTextResponse(43, "Бюджет до 900$, в Тбилиси и то дешевле"); err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profile.ID].Preferences
	if saved.City != cities.Batumi.Key || len(saved.LocationPreferences) != 1 {
		t.Fatalf("expected Batumi with its locations, got %+v", saved)
	}
	if len(builtCities) != 0 || store.searchProfiles[len(store.searchProfiles)-1].SearchArea != nil {
		t.Fatalf("expected the stored Batumi area to be kept without a rebuild, got %+v", builtCities)
	}
}

func TestMCPCitySwitchClearsPreviousCityLocations(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(44)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"city":"tbilisi"}`)
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON}))

	response, err := bot.handleTextResponse(44, "Теперь ищем в Тбилиси")
	if err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profile.ID].Preferences
	if saved.City != cities.Tbilisi.Key || len(saved.LocationPreferences) != 0 || saved.HasSearchArea {
		t.Fatalf("expected Tbilisi without Batumi locations, got %+v", saved)
	}
	last := store.searchProfiles[len(store.searchProfiles)-1]
	if last.SearchArea == nil || last.SearchArea.WKT != "" {
		t.Fatalf("expected explicitly empty search area, got %+v", last.SearchArea)
	}
	if !strings.Contains(response.Text, "Город поиска: Тбилиси") {
		t.Fatalf("expected city switch note, got %q", response.Text)
	}
}

func TestMCPCitySwitchDropsEchoedPreviousCityLocations(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(46)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"city":"tbilisi","location_preferences":[{"name":"Парк 6 мая","type":"radius","center_name":"6 May Park","radius_meters":1000,"boundary_street_name":null,"boundary_rule":null,"geometry_wkt":null,"google_place_id":null,"confidence":0.8,"raw_text":"","unresolved_rules":[]}]}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	response, err := bot.handleTextResponse(46, "Теперь ищем в Тбилиси")
	if err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profile.ID].Preferences
	if saved.City != cities.Tbilisi.Key || len(saved.LocationPreferences) != 0 {
		t.Fatalf("expected echoed Batumi locations to be dropped, got %+v", saved)
	}
	if len(builtCities) != 0 {
		t.Fatalf("expected no geometry build for dropped locations, got %+v", builtCities)
	}
	last := store.searchProfiles[len(store.searchProfiles)-1]
	if last.SearchArea == nil || last.SearchArea.WKT != "" || !strings.Contains(response.Text, "сброшены") {
		t.Fatalf("expected reset area and note, got %+v %q", last.SearchArea, response.Text)
	}
}

func TestMCPExplicitBatumiForLegacyProfileIsNotASwitch(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(47)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"city":"batumi","budget_max":700}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	response, err := bot.handleTextResponse(47, "Бюджет до 700$")
	if err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profile.ID].Preferences
	if saved.City != cities.Batumi.Key || len(saved.LocationPreferences) != 1 || strings.Contains(response.Text, "Город поиска") {
		t.Fatalf("expected unchanged Batumi search, got %+v %q", saved, response.Text)
	}
	if len(builtCities) != 0 || store.searchProfiles[len(store.searchProfiles)-1].SearchArea != nil {
		t.Fatalf("expected the stored Batumi area to be kept without a rebuild, got %+v", builtCities)
	}
}

func TestMCPCitySwitchWithNewLocationsBuildsNewCityGeometry(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(45)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"city":"tbilisi","location_preferences":[{"name":"Ваке","type":"radius","center_name":"Vake Park","radius_meters":1500,"boundary_street_name":null,"boundary_rule":null,"geometry_wkt":null,"google_place_id":null,"confidence":0.8,"raw_text":"Ваке","unresolved_rules":[]}]}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	if _, err := bot.handleTextResponse(45, "Ищем в Тбилиси, район Ваке"); err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profile.ID].Preferences
	if saved.City != cities.Tbilisi.Key || len(saved.LocationPreferences) != 1 || saved.LocationPreferences[0].CenterName != "Vake Park" {
		t.Fatalf("expected Tbilisi with Vake location, got %+v", saved)
	}
	if len(builtCities) != 1 || builtCities[0] != cities.Tbilisi.Key {
		t.Fatalf("expected Tbilisi geometry, got %+v", builtCities)
	}
}

func TestMCPRemovingLocationsClearsSearchArea(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(48)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"location_preferences":[]}`)
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(preferenceparser.PostGISGeometryBuilder{}))

	if _, err := bot.handleTextResponse(48, "Убери ограничение по району"); err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profile.ID].Preferences
	if len(saved.LocationPreferences) != 0 || saved.HasSearchArea {
		t.Fatalf("expected no locations and no area flag, got %+v", saved)
	}
	last := store.searchProfiles[len(store.searchProfiles)-1]
	if last.SearchArea == nil || last.SearchArea.WKT != "" {
		t.Fatalf("expected an explicitly empty search area, got %+v", last.SearchArea)
	}
}

func TestAwaitingConfirmationAcceptsGeoJSONSearchArea(t *testing.T) {
	store := newFakeProfileStore()
	parser := preferenceparser.Service{
		Normalizer: fakeNormalizer{normalized: "Ищу квартиру около парка 6 мая"},
		Extractor: fakeExtractor{prefs: domain.UserPreferences{
			BudgetMax: intPtr(650),
			LocationPreferences: []domain.LocationPreference{{
				CenterName:   "6 May Park",
				RadiusMeters: intPtr(1000),
				RawText:      "около парка 6 мая",
			}},
		}},
	}
	bot := New(
		"test-token",
		t.TempDir(),
		WithProfileStore(store),
		WithPreferencesParser(parser),
		WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON}),
	)

	if _, err := bot.HandleText(42, "/preferences"); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.HandleText(42, "Ищу квартиру до 650 около парка 6 мая"); err != nil {
		t.Fatal(err)
	}
	editedGeoJSON := `{
		"type": "FeatureCollection",
		"features": [{
			"type": "Feature",
			"properties": {},
			"geometry": {
				"type": "Polygon",
				"coordinates": [[
					[41.620000, 41.640000],
					[41.631000, 41.640000],
					[41.631000, 41.651000],
					[41.620000, 41.651000]
				]]
			}
		}]
	}`
	reply, err := bot.HandleText(42, editedGeoJSON)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Если зона на карте неправильная") || !strings.Contains(reply, cities.Batumi.GeoJSONEditorURL()) {
		t.Fatalf("expected updated preview with editor link, got %q", reply)
	}

	reply, err = bot.HandleText(42, "Сохранить")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Готово, фильтры сохранены.") {
		t.Fatalf("unexpected save reply %q", reply)
	}
	if len(store.searchProfiles) != 1 {
		t.Fatalf("expected search profile to be saved, got %d", len(store.searchProfiles))
	}
	area := store.searchProfiles[0].SearchArea
	if area == nil {
		t.Fatal("expected edited search area to be saved")
	}
	if !strings.HasPrefix(area.WKT, "MULTIPOLYGON(((") || !strings.Contains(area.WKT, "41.63100000 41.65100000") {
		t.Fatalf("unexpected WKT %q", area.WKT)
	}
	if !strings.Contains(area.GeoJSON, `"MultiPolygon"`) {
		t.Fatalf("unexpected GeoJSON %q", area.GeoJSON)
	}
}

func TestEditAreaReturnsCurrentGeoJSON(t *testing.T) {
	store := newFakeProfileStore()
	parser := preferenceparser.Service{
		Normalizer: fakeNormalizer{normalized: "Ищу квартиру около парка 6 мая"},
		Extractor: fakeExtractor{prefs: domain.UserPreferences{
			BudgetMax: intPtr(650),
			LocationPreferences: []domain.LocationPreference{{
				CenterName:   "6 May Park",
				RadiusMeters: intPtr(1000),
				RawText:      "около парка 6 мая",
			}},
		}},
	}
	bot := New(
		"test-token",
		t.TempDir(),
		WithProfileStore(store),
		WithPreferencesParser(parser),
		WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON}),
	)

	if _, err := bot.handleTextResponse(42, "/preferences"); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.handleTextResponse(42, "Ищу квартиру до 650 около парка 6 мая"); err != nil {
		t.Fatal(err)
	}
	response, err := bot.handleTextResponse(42, "Изменить зону")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Текущий GeoJSON зоны") ||
		!strings.Contains(response.Text, `"type": "MultiPolygon"`) ||
		!strings.Contains(response.Text, cities.Batumi.GeoJSONEditorURL()) {
		t.Fatalf("expected current GeoJSON edit instructions, got %q", response.Text)
	}
	if len(response.DocumentBytes) != 0 {
		t.Fatalf("short GeoJSON should be sent as text, got document %q", response.DocumentFilename)
	}
}

func TestEditAreaReturnsLargeCurrentGeoJSONAsDocument(t *testing.T) {
	area := &domain.GeoSearchArea{
		SRID:    4326,
		GeoJSON: `{"type":"MultiPolygon","coordinates":[[[[41.620000,41.640000],[41.621000,41.640000],[41.621000,41.641000],[41.620000,41.641000],[41.620000,41.640000]]]]}`,
	}
	for len(area.GeoJSON) < 5000 {
		area.GeoJSON = strings.Replace(area.GeoJSON, "]]]]}", "],[[[41.622000,41.642000],[41.623000,41.642000],[41.623000,41.643000],[41.622000,41.643000],[41.622000,41.642000]]]]}", 1)
	}

	response := searchAreaEditResponse(area, cities.Batumi)
	if response.DocumentFilename != "search-area.geojson" || len(response.DocumentBytes) == 0 {
		t.Fatalf("expected large GeoJSON document, got filename=%q bytes=%d", response.DocumentFilename, len(response.DocumentBytes))
	}
	if !strings.Contains(response.Text, "во вложении search-area.geojson") || !strings.Contains(response.Text, cities.Batumi.GeoJSONEditorURL()) {
		t.Fatalf("unexpected document caption %q", response.Text)
	}
}

func TestSubscribeChannelUsesProfileStore(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, err := bot.HandleText(42, "/subscribe https://t.me/rentals Batumi rentals")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Добавлен @rentals" {
		t.Fatalf("unexpected reply %q", reply)
	}
	profile, ok, err := store.GetProfile(context.Background(), "telegram_42")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(profile.Channels) != 1 {
		t.Fatalf("expected channel to be saved, got %+v", profile)
	}
	if profile.Channels[0].Source.Title != "Batumi rentals" {
		t.Fatalf("unexpected channel title %q", profile.Channels[0].Source.Title)
	}
}

func TestPlainChannelLinksAddSubscriptions(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, err := bot.HandleText(42, "https://t.me/batumi_rentals @rentals2 https://t.me/batumi_rentals/123")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Источники обновлены:") ||
		!strings.Contains(reply, "Добавлен @batumi_rentals") ||
		!strings.Contains(reply, "Добавлен @rentals2") ||
		!strings.Contains(reply, "не вступаю в чаты автоматически") {
		t.Fatalf("unexpected reply %q", reply)
	}
	profile, ok, err := store.GetProfile(context.Background(), "telegram_42")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || len(profile.Channels) != 2 {
		t.Fatalf("expected two channels to be saved, got %+v", profile)
	}
}

func TestPrivateInviteLinkExplainsManualLimitation(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, err := bot.HandleText(42, "https://t.me/+abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "только public Telegram-источники") ||
		!strings.Contains(reply, "Автоматическое вступление не выполняю") {
		t.Fatalf("unexpected reply %q", reply)
	}
}

func TestDebugModeCanBeToggled(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	// Default is true, so first toggle turns it off
	reply, err := bot.HandleText(42, "/debug")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Debug mode: off") {
		t.Fatalf("unexpected default debug reply %q", reply)
	}

	reply, err = bot.HandleText(42, "/debug off")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Debug mode: off") {
		t.Fatalf("unexpected disable reply %q", reply)
	}
	profile, ok, err := store.GetProfile(context.Background(), "telegram_42")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || profile.DebugMode == nil || *profile.DebugMode {
		t.Fatalf("expected debug mode to be disabled, got %+v", profile)
	}

	reply, err = bot.HandleText(42, "/debug on")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Debug mode: on") {
		t.Fatalf("unexpected enable reply %q", reply)
	}
	profile, _, _ = store.GetProfile(context.Background(), "telegram_42")
	if profile.DebugMode == nil || !*profile.DebugMode {
		t.Fatalf("expected debug mode to be enabled, got %+v", profile)
	}
}

func TestRepliesRemoveCommandKeyboardAndMenuListsStartSettingsAndReset(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	for _, command := range []string{"/start", "/channels"} {
		response, err := bot.handleTextResponse(42, command)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := response.ReplyMarkup.(*models.ReplyKeyboardRemove); !ok {
			t.Fatalf("expected %s to remove the reply keyboard, got %T", command, response.ReplyMarkup)
		}
	}
	var commands []string
	for _, command := range botMenuCommands {
		commands = append(commands, command.Command)
	}
	if strings.Join(commands, ",") != "start,settings,reset" {
		t.Fatalf("expected the menu to list start, settings and reset, got %v", commands)
	}
}

func TestClearAndResetCommands(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	// Setup a session and MCP history
	chatID := int64(42)
	profileID := "telegram_42"
	bot.setSession(chatID, &preferenceSession{State: "awaiting_query"})

	p := profiles.Profile{
		ID:             profileID,
		TelegramChatID: chatID,
		Preferences: &domain.UserPreferences{
			MinBudget:         400,
			MaxBudget:         800,
			RequireDishwasher: true,
			RequireOven:       true,
		},
		MCPHistory: []profiles.MCPMessage{
			{Role: "user", Content: "hello"},
		},
	}
	if err := store.UpsertProfile(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	store.searchProfiles = append(store.searchProfiles, domain.UserSearchProfile{
		UserID: profileID,
		Preferences: domain.UserPreferences{
			MinBudget:         400,
			MaxBudget:         800,
			RequireDishwasher: true,
			RequireOven:       true,
		},
	})

	// Send /clear
	response, err := bot.handleTextResponse(chatID, "/clear")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Сохраненные фильтры остались без изменений") {
		t.Fatalf("unexpected clear reply: %q", response.Text)
	}

	// Verify session and history are cleared, while filters remain intact.
	if bot.getSession(chatID) != nil {
		t.Fatal("expected session to be cleared")
	}
	profile, ok, err := store.GetProfile(context.Background(), profileID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected profile to exist")
	}
	if len(profile.MCPHistory) != 0 {
		t.Fatalf("expected MCP history to be empty, got %d messages", len(profile.MCPHistory))
	}
	if profile.Preferences == nil || !profile.Preferences.RequireDishwasher || !profile.Preferences.RequireOven {
		t.Fatalf("expected preferences to be preserved, got %+v", profile.Preferences)
	}
	if len(store.searchProfiles) != 1 {
		t.Fatalf("expected clear not to create empty search profiles, got %d", len(store.searchProfiles))
	}
}

func TestSaveReplyFeedbackStoresFeedback(t *testing.T) {
	store := newFakeProfileStore()
	store.feedbackMatched = true
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, handled, err := bot.saveReplyFeedback(context.Background(), &models.Message{
		ID:   101,
		Text: "гео определено неверно",
		Chat: models.Chat{ID: 42},
		ReplyToMessage: &models.Message{
			ID: 55,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !handled {
		t.Fatal("expected handled to be true")
	}
	if reply != "Отзыв сохранен: feedback #7" {
		t.Fatalf("unexpected reply %q", reply)
	}
	if len(store.feedbacks) != 1 {
		t.Fatalf("expected feedback to be saved, got %d", len(store.feedbacks))
	}
	got := store.feedbacks[0]
	if got.TelegramChatID != 42 || got.FeedbackMessageID != 101 || got.RepliedMessageID != 55 || got.Text != "гео определено неверно" {
		t.Fatalf("unexpected feedback: %+v", got)
	}
}

func TestReplyFeedbackGoesToAgentWithoutMCPMode(t *testing.T) {
	store := newFakeProfileStore()
	store.feedbackMatched = true
	store.profiles["telegram_42"] = profiles.Profile{ID: "telegram_42", TelegramChatID: 42, Preferences: &domain.UserPreferences{MaxBudget: 600}}
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Учту темный ремонт."}}]}`))
	}))
	defer agentServer.Close()
	t.Setenv("OPENAI_BASE_URL", agentServer.URL)
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)))

	reply, handled, err := bot.saveReplyFeedback(context.Background(), &models.Message{
		ID:             101,
		Text:           "слишком темно",
		Chat:           models.Chat{ID: 42},
		ReplyToMessage: &models.Message{ID: 55},
	})
	if err != nil || !handled || reply != "Учту темный ремонт." {
		t.Fatalf("expected the agent reply to feedback, got %q handled=%v err=%v", reply, handled, err)
	}
}

func TestSaveReplyFeedbackUnmatchedReplyReturnsNotHandled(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, handled, err := bot.saveReplyFeedback(context.Background(), &models.Message{
		ID:             101,
		Text:           "гео определено неверно",
		Chat:           models.Chat{ID: 42},
		ReplyToMessage: &models.Message{ID: 55},
	})
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		t.Fatal("expected handled to be false for unmatched reply")
	}
	if reply != "" {
		t.Fatalf("expected empty reply, got %q", reply)
	}
	if len(store.feedbacks) != 0 {
		t.Fatalf("unexpected feedback saved: %+v", store.feedbacks)
	}
}

type fakeNormalizer struct {
	normalized string
}

func (n fakeNormalizer) Normalize(ctx context.Context, input string) (string, error) {
	return n.normalized, nil
}

type fakeExtractor struct {
	prefs domain.UserPreferences
}

func (e fakeExtractor) Extract(ctx context.Context, normalizedInput string) (*domain.UserPreferences, error) {
	return &e.prefs, nil
}

type fakeGeometryBuilder struct {
	geoJSON string
	cities  *[]string
}

func (b fakeGeometryBuilder) BuildSearchGeometry(ctx context.Context, city cities.City, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error) {
	if b.cities != nil {
		*b.cities = append(*b.cities, city.Key)
	}
	return &domain.GeoSearchArea{SRID: 4326, GeoJSON: b.geoJSON}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func intPtr(value int) *int {
	return &value
}

func floatPtr(value float64) *float64 {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

func TestAllowedChatsFilter(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(),
		WithAllowedChats([]int64{123}),
		WithAllowedUsernames([]string{"spy_bots"}),
		WithProfileStore(store),
	)

	// 1. Chat ID not in allowed list, username not in allowed list
	{
		update := &models.Update{
			Message: &models.Message{
				Chat: models.Chat{ID: 456, Type: models.ChatTypePrivate},
				From: &models.User{ID: 456, Username: "other_user"},
				Text: "/start",
			},
		}
		// If it's not allowed, it should return early without panic.
		bot.handleUpdate(context.Background(), nil, update)
	}

	// 2. Chat ID in allowed list
	{
		update := &models.Update{
			Message: &models.Message{
				Chat: models.Chat{ID: 123, Type: models.ChatTypePrivate},
				From: &models.User{ID: 123, Username: "other_user"},
				Text: "/start",
			},
		}
		if bot.getSession(123) != nil {
			t.Fatal("session should be nil")
		}

		// Since we pass nil for api, the bot should proceed past the allowed filter and panic when trying to send a message.
		var panicked bool
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			bot.handleUpdate(context.Background(), nil, update)
		}()

		if !panicked {
			t.Fatal("expected update processing to proceed and panic due to nil api, but it did not panic")
		}

		session := bot.getSession(123)
		if session == nil || session.State != "awaiting_query" {
			t.Fatalf("expected session to be created for allowed chat, got: %v", session)
		}
	}

	// 3. Username in allowed list
	{
		update := &models.Update{
			Message: &models.Message{
				Chat: models.Chat{ID: 999, Type: models.ChatTypePrivate},
				From: &models.User{ID: 999, Username: "spy_bots"},
				Text: "/start",
			},
		}
		if bot.getSession(999) != nil {
			t.Fatal("session should be nil")
		}

		// Since we pass nil for api, the bot should proceed past the allowed filter and panic when trying to send a message.
		var panicked bool
		func() {
			defer func() {
				if r := recover(); r != nil {
					panicked = true
				}
			}()
			bot.handleUpdate(context.Background(), nil, update)
		}()

		if !panicked {
			t.Fatal("expected update processing to proceed and panic due to nil api, but it did not panic")
		}

		session := bot.getSession(999)
		if session == nil || session.State != "awaiting_query" {
			t.Fatalf("expected session to be created for allowed username, got: %v", session)
		}
	}
}

func TestPreferencesCommandStartsConversationInsteadOfWizard(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(49)
	profile.MCPMode = nil
	store.profiles[profile.ID] = profile
	agentCalls := 0
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agentCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Агент ответил."}}]}`))
	}))
	defer agentServer.Close()
	t.Setenv("OPENAI_BASE_URL", agentServer.URL)
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)))

	reply, err := bot.HandleText(49, "/preferences")
	if err != nil || reply != returningUserGreeting || bot.getSession(49) != nil {
		t.Fatalf("expected the returning greeting without a wizard session, got %q %v", reply, err)
	}
	if reply, err = bot.HandleText(49, "До 700$"); err != nil || reply != "Агент ответил." || agentCalls != 1 {
		t.Fatalf("expected the agent to answer, got %q %v after %d calls", reply, err, agentCalls)
	}
}

type fakeChannelAccess map[string]sources.ChannelAccess

func (f fakeChannelAccess) ChannelAccess(ctx context.Context, username string) (sources.ChannelAccess, error) {
	return f[username], nil
}

func TestSubscriptionReportsChannelAccess(t *testing.T) {
	store := newFakeProfileStore()
	checker := fakeChannelAccess{
		"joined_rent":  {Found: true, Title: "Joined Rent", Member: true},
		"foreign_rent": {Found: true, Title: "Foreign Rent"},
	}
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithChannelAccessChecker(checker))

	reply, err := bot.HandleText(42, "@joined_rent @foreign_rent @missing_rent")
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Добавлен @joined_rent\n",
		"Добавлен @foreign_rent. Аккаунт-читатель владельца бота не состоит в этом чате",
		"Не нашел публичный канал или группу @missing_rent",
	} {
		if !strings.Contains(reply, expected) {
			t.Fatalf("expected %q in reply:\n%s", expected, reply)
		}
	}
	profile, _, _ := store.GetProfile(context.Background(), "telegram_42")
	if len(profile.Channels) != 2 || profile.Channels[0].Source.Title != "Joined Rent" {
		t.Fatalf("expected two subscriptions with resolved titles, got %+v", profile.Channels)
	}
}

func TestMarkdownToHTMLKeepsUsernamesURLsAndBullets(t *testing.T) {
	cases := map[string]string{
		"Добавлен @definitely_not_a_real_channel_xyz123":                       "Добавлен @definitely_not_a_real_channel_xyz123",
		"https://t.me/rent_channel_one и @another_rent_channel":                "https://t.me/rent_channel_one и @another_rent_channel",
		"Районы:\n* Ваке\n* Сабуртало\n":                                       "Районы:\n* Ваке\n* Сабуртало\n",
		"**Бюджет**: до *700* $ и _важно_":                                     "<b>Бюджет</b>: до <i>700</i> $ и <i>важно</i>",
		"snake_case_name и a < b":                                              "snake_case_name и a &lt; b",
		"`code_with_underscores` [сайт](https://geojson.io/#map=13/41.7/44.8)": "<code>code_with_underscores</code> <a href=\"https://geojson.io/#map=13/41.7/44.8\">сайт</a>",
		"```json\n{\"a_b\": 1}\n```":                                           "<pre>{&#34;a_b&#34;: 1}\n</pre>",
		"## Итог":                                                              "<b>Итог</b>",
	}
	for input, want := range cases {
		if got := markdownToHTML(input); got != want {
			t.Errorf("markdownToHTML(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestMCPEditRebuildsLegacyLocationsWithoutStoredArea(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(50)
	profile.Preferences.HasSearchArea = false
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"budget_max":800}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	if _, err := bot.handleTextResponse(50, "Бюджет до 800$"); err != nil {
		t.Fatal(err)
	}
	if len(builtCities) != 1 || !store.profiles[profile.ID].Preferences.HasSearchArea {
		t.Fatalf("expected the missing zone to be built, got %+v", builtCities)
	}
}

func TestMCPEditKeepsHandDrawnAreaWithoutLocations(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(51)
	profile.Preferences.LocationPreferences = nil
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"budget_max":800,"location_preferences":[]}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	if _, err := bot.handleTextResponse(51, "Бюджет до 800$"); err != nil {
		t.Fatal(err)
	}
	if len(builtCities) != 0 || store.searchProfiles[len(store.searchProfiles)-1].SearchArea != nil {
		t.Fatalf("expected the hand-drawn area to be inherited, got %+v", builtCities)
	}
}

func TestExpiredPreferenceSessionIsDropped(t *testing.T) {
	bot := New("test-token", t.TempDir(), WithProfileStore(newFakeProfileStore()))
	bot.setSession(52, &preferenceSession{State: "awaiting_query"})
	bot.sessions[52].StartedAt = time.Now().Add(-preferenceSessionTTL - time.Minute)
	if bot.getSession(52) != nil {
		t.Fatal("expected an expired session to be dropped")
	}
}

func TestMCPEditWithEchoedLocationsKeepsStoredArea(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(53)
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"stop_words":["студия"],"location_preferences":[{"name":"Парк 6 мая","type":null,"center_name":"6 May Park","radius_meters":1000,"boundary_street_name":null,"boundary_rule":null,"geometry_wkt":null,"google_place_id":null,"confidence":0.7,"raw_text":"около парка","unresolved_rules":[]}]}`)
	var builtCities []string
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON, cities: &builtCities}))

	if _, err := bot.handleTextResponse(53, "Добавь стоп-слово студия"); err != nil {
		t.Fatal(err)
	}
	if len(builtCities) != 0 || store.searchProfiles[len(store.searchProfiles)-1].SearchArea != nil {
		t.Fatalf("expected the stored area to be kept, got %+v", builtCities)
	}
}

func TestStartGreetsNewUserAndRecordsGreetingInHistory(t *testing.T) {
	store := newFakeProfileStore()
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:1")
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)))

	reply, err := bot.HandleText(54, "/start")
	if err != nil || reply != newUserGreeting {
		t.Fatalf("expected the onboarding greeting, got %q %v", reply, err)
	}
	profile, ok := store.profiles[profiles.IDForTelegramChat(54)]
	if !ok || profile.TelegramChatID != 54 || bot.getSession(54) != nil {
		t.Fatalf("expected a profile without a wizard session, got %+v", profile)
	}
	history := profile.MCPHistory
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" || history[1].Content != newUserGreeting {
		t.Fatalf("expected the greeting turn in history, got %+v", history)
	}
}

func TestEnsureProfileDefaultsToMatchesOnlyAndRepairsMissingChatID(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	profile, err := bot.ensureProfile(context.Background(), 57)
	if err != nil || profile.TelegramChatID != 57 || profile.DebugMode == nil || *profile.DebugMode {
		t.Fatalf("expected a new matches-only profile with chat id, got %+v %v", profile, err)
	}
	allReports := true
	store.profiles["telegram_58"] = profiles.Profile{ID: "telegram_58", DebugMode: &allReports}
	profile, err = bot.ensureProfile(context.Background(), 58)
	if err != nil || profile.TelegramChatID != 58 || !*profile.DebugMode {
		t.Fatalf("expected the chat id repaired without touching debug mode, got %+v %v", profile, err)
	}
}

func TestNewUserMessageCreatesProfileAndReachesAgent(t *testing.T) {
	store := newFakeProfileStore()
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"В каком городе ищем?"}}]}`))
	}))
	defer agentServer.Close()
	t.Setenv("OPENAI_BASE_URL", agentServer.URL)
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)))

	reply, err := bot.HandleText(55, "Привет, ищу квартиру")
	if err != nil || reply != "В каком городе ищем?" {
		t.Fatalf("expected the agent reply, got %q %v", reply, err)
	}
	if _, ok := store.profiles[profiles.IDForTelegramChat(55)]; !ok {
		t.Fatal("expected the profile to be created")
	}
}

func TestAgentAddsSourcesThroughBot(t *testing.T) {
	store := newFakeProfileStore()
	requests := 0
	var toolResult string
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"add_sources","arguments":"{\"sources\":[\"@joined_rent\",\"t.me/+private\"]}"}}]}}]}`))
			return
		}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		toolResult = req.Messages[len(req.Messages)-1].Content
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Подключил."}}]}`))
	}))
	defer agentServer.Close()
	t.Setenv("OPENAI_BASE_URL", agentServer.URL)
	checker := fakeChannelAccess{"joined_rent": {Found: true, Title: "Joined Rent", Member: true}}
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithChannelAccessChecker(checker), WithMCPAgent(mcpagent.New("test-key", store)))

	if _, err := bot.HandleText(56, "@joined_rent и t.me/+private"); err != nil {
		t.Fatal(err)
	}
	channels := store.profiles[profiles.IDForTelegramChat(56)].Channels
	if len(channels) != 1 || channels[0].Source.Username != "joined_rent" || channels[0].Source.Title != "Joined Rent" {
		t.Fatalf("expected joined_rent to be subscribed, got %+v", channels)
	}
	if !strings.Contains(toolResult, "Добавлен @joined_rent") || !strings.Contains(toolResult, "t.me/+private: не подключен") {
		t.Fatalf("expected per-source results for the agent, got %q", toolResult)
	}
}

func TestPreferenceValueDisplaysUnknownSlugsInsteadOfHidingThem(t *testing.T) {
	if got := displayPreferenceValue("air_conditioning", "ru"); got != "кондиционер" {
		t.Fatalf("expected air conditioning label, got %q", got)
	}
	if got := displayPreferenceValue("heated_floor", "ru"); got != "heated floor" {
		t.Fatalf("expected the unknown slug to stay readable, got %q", got)
	}
}

func TestPreferenceValueTranslatesCommonAmenitiesAndFloorRules(t *testing.T) {
	cases := map[string]string{
		"balcony":         "балкон",
		"washing_machine": "стиральная машина",
		"not_first_floor": "не первый этаж",
		"no_last_floor":   "не последний этаж",
		"first_floor":     "первый этаж",
		"ground_floor":    "первый этаж",
		"light_floor":     "light floor",
	}
	for slug, want := range cases {
		if got := displayPreferenceValue(slug, "ru"); got != want {
			t.Fatalf("displayPreferenceValue(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestFirstSaveExplainsNextStepOnlyWhenSourcesExist(t *testing.T) {
	for _, withSources := range []bool{true, false} {
		store := newFakeProfileStore()
		profile := profiles.Profile{ID: "telegram_61", TelegramChatID: 61}
		if withSources {
			profile.Channels = []profiles.ChannelSubscription{{Source: sources.ChannelRef{ID: "batumi_rent", Username: "batumi_rent"}}}
		}
		store.profiles[profile.ID] = profile
		newScriptedAgentServer(t, `{"max_budget":600}`)
		bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)))

		response, err := bot.handleTextResponse(61, "да, запускай")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(response.Text, searchStartedNote); got != withSources {
			t.Fatalf("withSources=%v: unexpected next-step note in %q", withSources, response.Text)
		}
		if store.profiles[profile.ID].Preferences == nil {
			t.Fatal("expected preferences to be saved")
		}
	}
}

func TestLaterSavesDoNotRepeatNextStepNote(t *testing.T) {
	store := newFakeProfileStore()
	profile := batumiMCPProfile(62)
	profile.Channels = []profiles.ChannelSubscription{{Source: sources.ChannelRef{ID: "batumi_rent", Username: "batumi_rent"}}}
	store.profiles[profile.ID] = profile
	newScriptedAgentServer(t, `{"max_budget":700}`)
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithMCPAgent(mcpagent.New("test-key", store)))

	response, err := bot.handleTextResponse(62, "подними бюджет до 700")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Text, searchStartedNote) {
		t.Fatalf("expected no next-step note on a later edit, got %q", response.Text)
	}
}

type popularSourcesStore struct {
	*fakeProfileStore
	city      string
	profileID string
}

func (s *popularSourcesStore) PopularSources(ctx context.Context, profileID, city string, limit int) ([]profiles.PopularSource, error) {
	s.city, s.profileID = city, profileID
	if city != "tbilisi" {
		return nil, nil
	}
	return []profiles.PopularSource{{Username: "tbilisi_flats", Title: "Tbilisi Flats", Users: 3}}, nil
}

func TestSuggestSourcesUsesProfileCity(t *testing.T) {
	store := &popularSourcesStore{fakeProfileStore: newFakeProfileStore()}
	store.profiles["telegram_63"] = profiles.Profile{ID: "telegram_63", TelegramChatID: 63, Preferences: &domain.UserPreferences{City: "tbilisi"}}
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	result, err := bot.SuggestSources(context.Background(), 63, "")
	if err != nil {
		t.Fatal(err)
	}
	if store.city != "tbilisi" || store.profileID != "telegram_63" || !strings.Contains(result, "@tbilisi_flats — Tbilisi Flats") {
		t.Fatalf("unexpected suggestions %q for city %q", result, store.city)
	}
	store.profiles["telegram_64"] = profiles.Profile{ID: "telegram_64", TelegramChatID: 64}
	result, err = bot.SuggestSources(context.Background(), 64, "")
	if err != nil || store.city != "batumi" || !strings.Contains(result, "no popular sources") {
		t.Fatalf("expected an empty batumi answer, got %q %v", result, err)
	}
	if _, err := bot.SuggestSources(context.Background(), 64, "tbilisi"); err != nil || store.city != "tbilisi" {
		t.Fatalf("expected the agent city to win over the stored one, got %q %v", store.city, err)
	}
}

func TestSearchAreaMapHintKeepsLongRepliesUnchanged(t *testing.T) {
	long := strings.Repeat("а", 1020)
	if withSearchAreaMapHint(long) != long {
		t.Fatal("expected a long reply to stay unchanged")
	}
	if !strings.HasSuffix(withSearchAreaMapHint("Готово."), searchAreaMapHint) {
		t.Fatal("expected the map hint appended")
	}
}

func TestNewUserGreetingShowsExampleQuery(t *testing.T) {
	if !strings.Contains(newUserGreeting, "например: «Батуми, до 600$") {
		t.Fatalf("expected an example query in the greeting, got %q", newUserGreeting)
	}
}
