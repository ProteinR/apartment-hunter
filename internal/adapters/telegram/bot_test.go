package telegram

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"

	"github.com/go-telegram/bot/models"
)

const testSearchAreaGeoJSON = `{"type":"MultiPolygon","coordinates":[[[[41.620000,41.640000],[41.630000,41.640000],[41.630000,41.650000],[41.620000,41.650000],[41.620000,41.640000]]]]}`

type fakeProfileStore struct {
	profiles        map[string]profiles.Profile
	searchProfiles  []domain.UserSearchProfile
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

func (s *fakeProfileStore) SaveListingFeedback(ctx context.Context, feedback domain.ListingFeedback) (string, bool, error) {
	if !s.feedbackMatched {
		return "", false, nil
	}
	feedback.ID = "7"
	s.feedbacks = append(s.feedbacks, feedback)
	return feedback.ID, true, nil
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
		!strings.Contains(reply, "Теперь нужны источники объявлений.") ||
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
	if !strings.Contains(reply, "Если зона на карте неправильная") || !strings.Contains(reply, batumiGeoJSONEditorURL) {
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
		!strings.Contains(response.Text, batumiGeoJSONEditorURL) {
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

	response := searchAreaEditResponse(area)
	if response.DocumentFilename != "search-area.geojson" || len(response.DocumentBytes) == 0 {
		t.Fatalf("expected large GeoJSON document, got filename=%q bytes=%d", response.DocumentFilename, len(response.DocumentBytes))
	}
	if !strings.Contains(response.Text, "во вложении search-area.geojson") || !strings.Contains(response.Text, batumiGeoJSONEditorURL) {
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

func TestRepliesIncludeCommandKeyboard(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	response, err := bot.handleTextResponse(42, "/start")
	if err != nil {
		t.Fatal(err)
	}
	keyboard, ok := response.ReplyMarkup.(*models.ReplyKeyboardMarkup)
	if !ok {
		t.Fatalf("expected command keyboard, got %T", response.ReplyMarkup)
	}
	for _, expected := range []string{"/preferences", "/channels", "/subscribe", "/unsubscribe", "/debug", "/mcp"} {
		if !keyboardHasButton(keyboard, expected) {
			t.Fatalf("expected keyboard to contain %q: %+v", expected, keyboard.Keyboard)
		}
	}

	response, err = bot.handleTextResponse(42, "/channels")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := response.ReplyMarkup.(*models.ReplyKeyboardMarkup); !ok {
		t.Fatalf("expected command keyboard after /channels, got %T", response.ReplyMarkup)
	}
}

func TestSaveReplyFeedbackStoresFeedback(t *testing.T) {
	store := newFakeProfileStore()
	store.feedbackMatched = true
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, err := bot.saveReplyFeedback(context.Background(), &models.Message{
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

func TestSaveReplyFeedbackExplainsUnmatchedReply(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, err := bot.saveReplyFeedback(context.Background(), &models.Message{
		ID:             101,
		Text:           "гео определено неверно",
		Chat:           models.Chat{ID: 42},
		ReplyToMessage: &models.Message{ID: 55},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Не нашел объявление") {
		t.Fatalf("unexpected reply %q", reply)
	}
	if len(store.feedbacks) != 0 {
		t.Fatalf("unexpected feedback saved: %+v", store.feedbacks)
	}
}

func keyboardHasButton(keyboard *models.ReplyKeyboardMarkup, text string) bool {
	for _, row := range keyboard.Keyboard {
		for _, button := range row {
			if button.Text == text {
				return true
			}
		}
	}
	return false
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
}

func (b fakeGeometryBuilder) BuildSearchGeometry(ctx context.Context, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error) {
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
				Chat: models.Chat{ID: 456},
				From: &models.User{Username: "other_user"},
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
				Chat: models.Chat{ID: 123},
				From: &models.User{Username: "other_user"},
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
				Chat: models.Chat{ID: 999},
				From: &models.User{Username: "spy_bots"},
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
