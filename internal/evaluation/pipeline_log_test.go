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

func TestFilePipelineLoggerWritesJSONL(t *testing.T) {
	now := time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC)
	logger := &FilePipelineLogger{
		Dir: t.TempDir(),
		Now: func() time.Time { return now },
	}

	err := logger.LogPipelineEvent(context.Background(), PipelineLogEvent{
		Source: domain.ListingSourceRef{
			Source:         "telegram",
			ProfileID:      "telegram_42",
			TelegramChatID: 42,
			ChannelID:      "2967368875",
			ChannelName:    "Rentals",
			MessageID:      3764,
			MessageLink:    "https://t.me/example/3764",
		},
		ListingID:  "2967368875:3764",
		Stage:      "basic_filter",
		Status:     "passed",
		Price:      700,
		AreaM2:     56,
		DurationMS: 12,
	})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(logger.Dir, "pipeline-2026-06-08.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var event PipelineLogEvent
	if err := json.Unmarshal(data[:len(data)-1], &event); err != nil {
		t.Fatal(err)
	}
	if !event.Time.Equal(now) {
		t.Fatalf("unexpected time %s", event.Time)
	}
	if event.Source.ProfileID != "telegram_42" || event.Source.MessageID != 3764 {
		t.Fatalf("missing source context: %+v", event.Source)
	}
	if event.Stage != "basic_filter" || event.Status != "passed" {
		t.Fatalf("unexpected stage/status: %+v", event)
	}
	if event.Price != 700 || event.AreaM2 != 56 || event.DurationMS != 12 {
		t.Fatalf("unexpected listing/debug fields: %+v", event)
	}
}

func TestFilePipelineLoggerRemovesLogsOlderThanRetention(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pipeline-2026-06-04.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pipeline-2026-06-06.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := &FilePipelineLogger{
		Dir:       dir,
		Retention: 72 * time.Hour,
		Now:       func() time.Time { return time.Date(2026, 6, 8, 10, 30, 0, 0, time.UTC) },
	}
	if err := logger.LogPipelineEvent(context.Background(), PipelineLogEvent{Stage: "received"}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(dir, "pipeline-2026-06-04.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("expected old log to be removed, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "pipeline-2026-06-06.jsonl")); err != nil {
		t.Fatalf("expected recent log to remain: %v", err)
	}
}
