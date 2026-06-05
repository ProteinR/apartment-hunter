package profiles

import (
	"path/filepath"
	"testing"

	"apartment-hunter/internal/sources"
)

func TestConfigUpsertKeepsExistingChannelsWhenSavingPreferences(t *testing.T) {
	cfg := Config{}
	cfg.Upsert(Profile{
		ID:              "telegram_42",
		TelegramChatID:  42,
		PreferencesPath: "old.yaml",
		Channels: []ChannelSubscription{
			{Source: sources.ChannelRef{ID: "rentals", Username: "rentals"}},
		},
	})

	cfg.Upsert(Profile{
		ID:              "telegram_42",
		TelegramChatID:  42,
		PreferencesPath: "new.yaml",
	})

	profile, ok := cfg.Find("telegram_42")
	if !ok {
		t.Fatal("expected profile")
	}
	if profile.PreferencesPath != "new.yaml" {
		t.Fatalf("unexpected preferences path %q", profile.PreferencesPath)
	}
	if len(profile.Channels) != 1 {
		t.Fatalf("expected existing channel to be preserved, got %+v", profile.Channels)
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	input := Config{Profiles: []Profile{{
		ID:              "telegram_42",
		TelegramChatID:  42,
		PreferencesPath: "preferences/preferences_42.yaml",
		Channels: []ChannelSubscription{{
			Source: sources.ChannelRef{ID: "rentals", Username: "rentals"},
			Cursor: sources.Cursor{LastMessageID: 10},
		}},
	}}}

	if err := Save(path, input); err != nil {
		t.Fatal(err)
	}
	output, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Profiles) != 1 || output.Profiles[0].Channels[0].Cursor.LastMessageID != 10 {
		t.Fatalf("unexpected output %+v", output)
	}
}
