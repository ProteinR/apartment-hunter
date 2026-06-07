package telegram

import (
	"context"
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
)

type fakeProfileStore struct {
	profiles       map[string]profiles.Profile
	searchProfiles []domain.UserSearchProfile
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

type fakeGeometryBuilder struct{}

func (fakeGeometryBuilder) BuildSearchGeometry(ctx context.Context, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error) {
	return &domain.GeoSearchArea{SRID: 4326}, nil
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
