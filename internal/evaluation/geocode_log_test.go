package evaluation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"apartment-hunter/internal/domain"
)

func TestFileGeocodeLoggerWritesJSONL(t *testing.T) {
	now := time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC)
	logger := &FileGeocodeLogger{
		Dir: t.TempDir(),
		Now: func() time.Time { return now },
	}

	err := logger.LogGeocodeAttempt(context.Background(), GeocodeLogEvent{
		Source: domain.ListingSourceRef{
			Source:         "telegram",
			ProfileID:      "telegram_42",
			TelegramChatID: 42,
			ChannelID:      "2967368875",
			ChannelName:    "Rentals",
			MessageID:      3764,
			MessageLink:    "https://t.me/example/3764",
			GroupedID:      123,
		},
		ListingID:         "2967368875:3764",
		ListingTitle:      "Telegram listing",
		Query:             "улица Грибоедова,21, Batumi, Georgia",
		MaxDistanceMeters: 15000,
		ResultName:        "21 Alexander Griboedov St, Batumi, Georgia",
		ResultLatitude:    41.6466,
		ResultLongitude:   41.6266,
		Success:           true,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(logger.Dir, "geocoding-2026-06-08.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var event GeocodeLogEvent
	if err := json.Unmarshal(data[:len(data)-1], &event); err != nil {
		t.Fatal(err)
	}
	if !event.Time.Equal(now) {
		t.Fatalf("unexpected time %s", event.Time)
	}
	if event.Source.ProfileID != "telegram_42" || event.Source.TelegramChatID != 42 {
		t.Fatalf("missing source context: %+v", event.Source)
	}
	if event.ListingID != "2967368875:3764" || event.Query != "улица Грибоедова,21, Batumi, Georgia" {
		t.Fatalf("unexpected event %+v", event)
	}
	if event.ResultLatitude != 41.6466 || event.ResultLongitude != 41.6266 {
		t.Fatalf("unexpected result coordinates %.6f %.6f", event.ResultLatitude, event.ResultLongitude)
	}
}

func TestFileGeocodeLoggerRemovesLogsOlderThanRetention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "geocoding-2026-06-04.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "geocoding-2026-06-06.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := &FileGeocodeLogger{
		Dir:       dir,
		Retention: 72 * time.Hour,
		Now:       func() time.Time { return time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC) },
	}
	if err := logger.LogGeocodeAttempt(context.Background(), GeocodeLogEvent{Query: "Europe Square"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "geocoding-2026-06-04.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("expected old log to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "geocoding-2026-06-06.jsonl")); err != nil {
		t.Fatalf("expected recent log to remain: %v", err)
	}
}
