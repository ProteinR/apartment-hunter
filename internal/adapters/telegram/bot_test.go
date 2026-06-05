package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"apartment-hunter/internal/profiles"
)

func TestHandleTextRunsInterviewAndSavesPreferences(t *testing.T) {
	outputDir := t.TempDir()
	bot := New("test-token", outputDir)
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

	if _, err := os.Stat(filepath.Join(outputDir, "preferences_42.yaml")); err != nil {
		t.Fatalf("expected yaml preferences to be saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outputDir, "preferences_42.json")); err != nil {
		t.Fatalf("expected json preferences to be saved: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(outputDir, "profiles.json"))
	if err != nil {
		t.Fatalf("expected profiles to be saved: %v", err)
	}
	var cfg profiles.Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	profile, ok := cfg.Find("telegram_42")
	if !ok {
		t.Fatal("expected telegram profile")
	}
	if profile.PreferencesPath != filepath.Join(outputDir, "preferences_42.yaml") {
		t.Fatalf("unexpected preferences path %q", profile.PreferencesPath)
	}
}
