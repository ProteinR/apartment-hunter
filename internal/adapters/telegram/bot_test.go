package telegram

import (
	"context"
	"testing"

	"apartment-hunter/internal/profiles"
)

type fakeProfileStore struct {
	profiles map[string]profiles.Profile
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

func TestHandleTextRunsInterviewAndSavesPreferences(t *testing.T) {
	outputDir := t.TempDir()
	store := newFakeProfileStore()
	bot := New("test-token", outputDir, WithProfileStore(store))
	chatID := int64(42)

	reply, err := bot.HandleText(chatID, "/start")
	if err != nil {
		t.Fatal(err)
	}
	if reply == "" {
		t.Fatal("expected start reply")
	}

	answers := []string{
		"400", "1200", "45", "y", "y", "1000", "y", "n", "Old Boulevard",
		"minimalist", "y", "n", "y", "n", "5", "4", "3", "2", "1",
	}

	for _, answer := range answers {
		reply, err = bot.HandleText(chatID, answer)
		if err != nil {
			t.Fatal(err)
		}
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
	if profile.Preferences.MaxBudget != 1200 {
		t.Fatalf("unexpected max budget %d", profile.Preferences.MaxBudget)
	}
}

func TestSubscribeChannelUsesProfileStore(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, err := bot.HandleText(42, "/subscribe @rentals Batumi rentals")
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Subscribed to @rentals" {
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
