package telegram

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
)

const widgetTestChatID int64 = 4242

func widgetTestPreferences() domain.UserPreferences {
	return domain.UserPreferences{
		City:              "batumi",
		MinBudget:         400,
		BudgetMin:         intPtr(400),
		MaxBudget:         800,
		BudgetMax:         intPtr(800),
		MinAreaM2:         45,
		AreaMin:           floatPtr(45),
		RequireDishwasher: true,
		RequiredAmenities: []string{"dishwasher", "balcony"},
		HasSearchArea:     true,
		LocationPreferences: []domain.LocationPreference{
			{Name: "Новый бульвар", CenterName: "Новый бульвар", MaxDistanceMeters: 1500},
			{Name: "6 Мая парк", CenterName: "6 Мая парк", MaxDistanceMeters: 1500},
		},
	}
}

func newWidgetTestBot(t *testing.T, prefs *domain.UserPreferences) (*Bot, *fakeProfileStore) {
	t.Helper()
	store := newFakeProfileStore()
	profileID := profiles.IDForTelegramChat(widgetTestChatID)
	store.profiles[profileID] = profiles.Profile{ID: profileID, TelegramChatID: widgetTestChatID, Preferences: prefs}
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithGeometryBuilder(fakeGeometryBuilder{geoJSON: testSearchAreaGeoJSON}))
	return bot, store
}

func widgetStartText(prefs domain.UserPreferences, budget, area, mustHave, keep int) string {
	return fmt.Sprintf("/start set_b%da%dm%dk%dv%s", budget, area, mustHave, keep, widgetDistrictsChecksum(prefs))
}

func TestParseWidgetSettingsPayloadRejectsMalformedValues(t *testing.T) {
	valid := "/start set_b700a50m3k1v0a1b2c3d"
	if settings, ok := parseWidgetSettingsPayload(valid); !ok || settings.MaxBudget != 700 || settings.MinAreaM2 != 50 || settings.MustHave != 3 || settings.KeepDistricts != 1 || settings.DistrictsChecksum != "0a1b2c3d" {
		t.Fatalf("expected a parsed payload, got %+v %v", settings, ok)
	}
	for _, text := range []string{
		"/start set_b700a50m3k1",
		"/start set_b700a50m4k1v0a1b2c3d",
		"/start set_b100000a50m3k1v0a1b2c3d",
		"/start set_b700a1000m3k1v0a1b2c3d",
		"/start set_b700a50m3k4096v0a1b2c3d",
		"/start set_b700a50m3k1v0A1B2C3D",
		"/start set_b-1a50m3k1v0a1b2c3d",
		"/start set_b700a50m3k1v0a1b2c3d extra",
	} {
		if _, ok := parseWidgetSettingsPayload(text); ok {
			t.Fatalf("expected %q to be rejected", text)
		}
	}
}

func TestWidgetSettingsStartAppliesChangesWithoutAgent(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, store := newWidgetTestBot(t, &prefs)
	response, err := bot.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 700, 50, mustHaveOvenBit, 1))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Сохранил настройки из виджета", "Бюджет до: $800 → $700", "Площадь от: 45 м² → 50 м²", "Посудомойка: не обязательно", "Духовка: обязательно", "Убрал из поиска: 6 Мая парк"} {
		if !strings.Contains(response.Text, want) {
			t.Fatalf("expected reply to contain %q, got %q", want, response.Text)
		}
	}
	saved := store.profiles[profiles.IDForTelegramChat(widgetTestChatID)].Preferences
	if saved == nil || saved.MaxBudget != 700 || saved.BudgetMax == nil || *saved.BudgetMax != 700 || saved.MinBudget != 400 {
		t.Fatalf("unexpected budget after save: %+v", saved)
	}
	if saved.MinAreaM2 != 50 || saved.AreaMin == nil || *saved.AreaMin != 50 {
		t.Fatalf("unexpected area after save: %+v", saved)
	}
	if saved.RequireDishwasher || !saved.RequireOven || strings.Join(saved.RequiredAmenities, ",") != "balcony,oven" {
		t.Fatalf("unexpected amenities after save: dishwasher=%v oven=%v %v", saved.RequireDishwasher, saved.RequireOven, saved.RequiredAmenities)
	}
	if len(saved.LocationPreferences) != 1 || saved.LocationPreferences[0].Name != "Новый бульвар" || len(saved.Locations) != 1 {
		t.Fatalf("unexpected districts after save: %+v / %+v", saved.LocationPreferences, saved.Locations)
	}
	if len(store.searchProfiles) != 1 || store.searchProfiles[0].SearchArea == nil || store.searchProfiles[0].SearchArea.GeoJSON == "" {
		t.Fatalf("expected a rebuilt search area to be saved, got %+v", store.searchProfiles)
	}
	history := store.profiles[profiles.IDForTelegramChat(widgetTestChatID)].MCPHistory
	if len(history) != 2 || !strings.Contains(history[1].Content, "Сохранил настройки из виджета") {
		t.Fatalf("expected the widget save to be recorded in agent history, got %+v", history)
	}
}

func TestWidgetSettingsStartClearsLimitsAndLowersMinimumBudget(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, store := newWidgetTestBot(t, &prefs)
	if _, err := bot.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 300, 0, mustHaveDishwasherBit, 3)); err != nil {
		t.Fatal(err)
	}
	saved := store.profiles[profiles.IDForTelegramChat(widgetTestChatID)].Preferences
	if saved.MaxBudget != 300 || saved.MinBudget != 0 || saved.BudgetMin != nil {
		t.Fatalf("expected the minimum budget above the new maximum to be cleared, got %+v", saved)
	}
	if saved.MinAreaM2 != 0 || saved.AreaMin != nil {
		t.Fatalf("expected the area limit to be cleared, got %+v", saved)
	}

	prefs = *saved
	if _, err := bot.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 0, 0, mustHaveDishwasherBit, 3)); err != nil {
		t.Fatal(err)
	}
	saved = store.profiles[profiles.IDForTelegramChat(widgetTestChatID)].Preferences
	if saved.MaxBudget != 0 || saved.BudgetMax != nil {
		t.Fatalf("expected the budget limit to be cleared, got %+v", saved)
	}
}

func TestWidgetSettingsStartRejectsStaleOrMalformedPayload(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, store := newWidgetTestBot(t, &prefs)
	response, err := bot.handleTextResponse(widgetTestChatID, "/start set_b700a50m0k3vdeadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != widgetSettingsStaleText {
		t.Fatalf("expected stale reply, got %q", response.Text)
	}
	response, err = bot.handleTextResponse(widgetTestChatID, "/start set_b700")
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != widgetSettingsMalformedText {
		t.Fatalf("expected malformed reply, got %q", response.Text)
	}
	saved := store.profiles[profiles.IDForTelegramChat(widgetTestChatID)].Preferences
	if saved.MaxBudget != 800 || len(store.searchProfiles) != 0 {
		t.Fatalf("expected nothing to change, got %+v and %d saves", saved, len(store.searchProfiles))
	}
}

func TestWidgetSettingsStartReportsUnchangedAndMissingPreferences(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, store := newWidgetTestBot(t, &prefs)
	response, err := bot.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 800, 45, mustHaveDishwasherBit, 3))
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != widgetSettingsUnchangedText || len(store.searchProfiles) != 0 {
		t.Fatalf("expected an unchanged reply without saving, got %q and %d saves", response.Text, len(store.searchProfiles))
	}

	empty, _ := newWidgetTestBot(t, nil)
	response, err = empty.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 800, 45, 0, 3))
	if err != nil {
		t.Fatal(err)
	}
	if response.Text != widgetSettingsNoPrefsText {
		t.Fatalf("expected a no-preferences reply, got %q", response.Text)
	}
}

func TestSettingsCommandBuildsWidgetWithProfileValues(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, _ := newWidgetTestBot(t, &prefs)
	bot.username = "Apartmenthuner_bot"
	response, err := bot.handleTextResponse(widgetTestChatID, "/settings")
	if err != nil {
		t.Fatal(err)
	}
	if response.Widget == nil {
		t.Fatalf("expected a widget, got %+v", response)
	}
	var data settingsWidgetData
	if err := json.Unmarshal([]byte(response.Widget.ChatData), &data); err != nil {
		t.Fatal(err)
	}
	want := settingsWidgetData{City: "Батуми", MaxBudget: 800, MinAreaM2: 45, MustHave: mustHaveDishwasherBit, Districts: []string{"Новый бульвар", "6 Мая парк"}, DistrictChecksum: widgetDistrictsChecksum(prefs), BotUsername: "Apartmenthuner_bot"}
	got, _ := json.Marshal(data)
	expected, _ := json.Marshal(want)
	if !bytes.Equal(got, expected) {
		t.Fatalf("unexpected chat data:\n got %s\nwant %s", got, expected)
	}
	if !strings.Contains(response.Text, "Бюджет до: $800") {
		t.Fatalf("expected a text fallback with current values, got %q", response.Text)
	}
}

func TestSettingsWidgetHTMLFitsTelegramLimits(t *testing.T) {
	if len(searchSettingsWidgetHTML) > 32768 {
		t.Fatalf("widget html is %d bytes", len(searchSettingsWidgetHTML))
	}
	var packed bytes.Buffer
	writer, _ := zlib.NewWriterLevel(&packed, zlib.BestCompression)
	_, _ = writer.Write([]byte(searchSettingsWidgetHTML))
	_ = writer.Close()
	if packed.Len() > 3800 {
		t.Fatalf("widget html compresses to %d bytes, keep it under 3800", packed.Len())
	}
	for _, unexpected := range []string{"Кондиционер", "Балкон", "Apartmenthuner_bot"} {
		if strings.Contains(searchSettingsWidgetHTML, unexpected) {
			t.Fatalf("widget html should not contain %q", unexpected)
		}
	}
}

func TestSettingsWidgetChatDataStaysWithinLimitForManyLongDistricts(t *testing.T) {
	prefs := widgetTestPreferences()
	prefs.LocationPreferences = nil
	for i := 0; i < 20; i++ {
		prefs.LocationPreferences = append(prefs.LocationPreferences, domain.LocationPreference{Name: fmt.Sprintf("Очень длинное название района номер %d у моря", i)})
	}
	bot, _ := newWidgetTestBot(t, &prefs)
	bot.username = "Apartmenthuner_bot"
	widget, err := bot.buildSettingsWidget(context.Background(), profiles.Profile{Preferences: &prefs})
	if err != nil {
		t.Fatal(err)
	}
	var data settingsWidgetData
	if err := json.Unmarshal([]byte(widget.ChatData), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Districts) != maxWidgetDistricts || len(widget.ChatData) > 1024 {
		t.Fatalf("expected %d districts within 1024 bytes, got %d districts and %d bytes", maxWidgetDistricts, len(data.Districts), len(widget.ChatData))
	}
	next, changes := applyWidgetSettings(prefs, widgetSettings{MaxBudget: 800, MinAreaM2: 45, MustHave: mustHaveDishwasherBit, KeepDistricts: (1 << maxWidgetDistricts) - 2})
	if len(next.LocationPreferences) != 19 || len(changes) != 1 {
		t.Fatalf("expected only the first district removed and hidden ones kept, got %d districts and %v", len(next.LocationPreferences), changes)
	}
}

func TestSendSettingsWidgetPostsToBetaEndpoint(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, _ := newWidgetTestBot(t, &prefs)
	var widgetRequest map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getMe"):
			_, _ = w.Write([]byte(`{"ok":true,"result":{"username":"Apartmenthuner_bot"}}`))
		case strings.HasSuffix(r.URL.Path, "/sendWidget"):
			if r.URL.Path != "/beta/bottest-token/sendWidget" {
				t.Errorf("unexpected widget path %s", r.URL.Path)
			}
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &widgetRequest)
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	bot.widgetEndpoint = server.URL + "/beta/bot%s/sendWidget"
	bot.getMeEndpoint = server.URL + "/bot%s/getMe"
	result, err := bot.SendSettingsWidget(context.Background(), widgetTestChatID)
	if err != nil {
		t.Fatal(err)
	}
	if result != "settings widget sent to the chat" {
		t.Fatalf("unexpected result %q", result)
	}
	if widgetRequest["chat_id"] != float64(widgetTestChatID) || widgetRequest["html"] != searchSettingsWidgetHTML || !strings.Contains(fmt.Sprint(widgetRequest["chat_data"]), `"u":"Apartmenthuner_bot"`) {
		t.Fatalf("unexpected widget request %+v", widgetRequest)
	}
}

func TestPostWidgetErrorDoesNotLeakToken(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, _ := newWidgetTestBot(t, &prefs)
	bot.widgetEndpoint = "http://127.0.0.1:1/beta/bot%s/sendWidget"
	err := bot.postWidget(context.Background(), widgetTestChatID, settingsWidget{HTML: "<p>x</p>"})
	if err == nil || strings.Contains(err.Error(), "test-token") {
		t.Fatalf("expected an error without the token, got %v", err)
	}
}

func TestWidgetSettingsKeepsUntouchedFractionalArea(t *testing.T) {
	prefs := widgetTestPreferences()
	prefs.MinAreaM2 = 45.5
	prefs.AreaMin = floatPtr(45.5)
	bot, store := newWidgetTestBot(t, &prefs)
	bot.username = "Apartmenthuner_bot"
	widget, err := bot.buildSettingsWidget(context.Background(), profiles.Profile{Preferences: &prefs})
	if err != nil {
		t.Fatal(err)
	}
	var data settingsWidgetData
	_ = json.Unmarshal([]byte(widget.ChatData), &data)
	response, err := bot.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 700, data.MinAreaM2, mustHaveDishwasherBit, 3))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(response.Text, "Площадь") {
		t.Fatalf("expected the untouched area to stay unchanged, got %q", response.Text)
	}
	saved := store.profiles[profiles.IDForTelegramChat(widgetTestChatID)].Preferences
	if saved.MinAreaM2 != 45.5 || saved.MaxBudget != 700 {
		t.Fatalf("unexpected preferences %+v", saved)
	}
}

func TestWidgetSettingsRemovingAllDistrictsClearsSearchArea(t *testing.T) {
	prefs := widgetTestPreferences()
	prefs.UnresolvedLocationRules = []string{"center unresolved: 6 Мая парк", "не первый этаж"}
	store := newFakeProfileStore()
	profileID := profiles.IDForTelegramChat(widgetTestChatID)
	store.profiles[profileID] = profiles.Profile{ID: profileID, TelegramChatID: widgetTestChatID, Preferences: &prefs}
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithGeometryBuilder(fakeGeometryBuilder{}))
	response, err := bot.handleTextResponse(widgetTestChatID, widgetStartText(prefs, 800, 45, mustHaveDishwasherBit, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "Убрал из поиска: Новый бульвар, 6 Мая парк") {
		t.Fatalf("unexpected reply %q", response.Text)
	}
	saved := store.profiles[profileID].Preferences
	if len(saved.LocationPreferences) != 0 || len(saved.Locations) != 0 || saved.HasSearchArea {
		t.Fatalf("expected no districts and no search area, got %+v", saved)
	}
	if strings.Join(saved.UnresolvedLocationRules, ",") != "не первый этаж" {
		t.Fatalf("expected rules of removed districts to be dropped, got %v", saved.UnresolvedLocationRules)
	}
	if len(store.searchProfiles) != 1 || store.searchProfiles[0].SearchArea == nil || store.searchProfiles[0].SearchArea.GeoJSON != "" {
		t.Fatalf("expected an empty search area to be saved, got %+v", store.searchProfiles)
	}
}

func TestSendSettingsWidgetFallsBackToSummaryWhenTelegramRejects(t *testing.T) {
	prefs := widgetTestPreferences()
	bot, _ := newWidgetTestBot(t, &prefs)
	bot.username = "Apartmenthuner_bot"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: widgets are not available"}`))
	}))
	defer server.Close()
	bot.widgetEndpoint = server.URL + "/beta/bot%s/sendWidget"
	result, err := bot.SendSettingsWidget(context.Background(), widgetTestChatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result, "widget could not be sent, tell the user: ") || !strings.Contains(result, "Бюджет до: $800") {
		t.Fatalf("expected a text summary for the agent, got %q", result)
	}
}

func TestSettingsWidgetClampsValuesAboveParseLimits(t *testing.T) {
	prefs := widgetTestPreferences()
	prefs.MaxBudget = 150000
	prefs.BudgetMax = intPtr(150000)
	bot, _ := newWidgetTestBot(t, &prefs)
	bot.username = "Apartmenthuner_bot"
	widget, err := bot.buildSettingsWidget(context.Background(), profiles.Profile{Preferences: &prefs})
	if err != nil {
		t.Fatal(err)
	}
	var data settingsWidgetData
	_ = json.Unmarshal([]byte(widget.ChatData), &data)
	if _, ok := parseWidgetSettingsPayload(widgetStartText(prefs, data.MaxBudget, data.MinAreaM2, data.MustHave, 3)); !ok {
		t.Fatalf("expected the widget's own values to parse, got budget %d", data.MaxBudget)
	}
}
