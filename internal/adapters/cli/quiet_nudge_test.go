package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"apartment-hunter/internal/profiles"
)

type fakeQuietSearchStore struct {
	quiet  []profiles.QuietSearch
	marked []string
}

func (s *fakeQuietSearchStore) QuietSearches(ctx context.Context, quietFor, maxQuiet time.Duration) ([]profiles.QuietSearch, error) {
	if quietFor != quietSearchThreshold || maxQuiet != quietSearchMaxAge {
		return nil, errors.New("unexpected threshold")
	}
	return s.quiet, nil
}

func (s *fakeQuietSearchStore) MarkQuietNudgeSent(ctx context.Context, profileID string, quietSince time.Time) error {
	s.marked = append(s.marked, profileID)
	return nil
}

func TestQuietSearchNudgeSendsOnceAndMarksEvenWhenSendFails(t *testing.T) {
	store := &fakeQuietSearchStore{quiet: []profiles.QuietSearch{
		{ProfileID: "telegram_1", TelegramChatID: 1, StatusCounts: map[string]int{"rejected_basic": 4, "rejected_location": 2, "rejected_non_listing": 9}},
		{ProfileID: "telegram_2", TelegramChatID: 2},
	}}
	sent := map[int64]string{}
	nudger := quietSearchNudger{store: store, now: daytime, send: func(ctx context.Context, chatID int64, text string) error {
		sent[chatID] = text
		if chatID == 2 {
			return errors.New("bot blocked")
		}
		return nil
	}}

	nudger.NudgeQuietSearches(context.Background())

	if !strings.Contains(sent[1], "я проверил 6") || !strings.Contains(sent[1], "цена, площадь или обязательные удобства: 4") || !strings.Contains(sent[1], "вне зоны поиска: 2") {
		t.Fatalf("unexpected nudge %q", sent[1])
	}
	if !strings.Contains(sent[2], "не было новых объявлений") {
		t.Fatalf("expected the empty-sources nudge, got %q", sent[2])
	}
	if strings.Join(store.marked, ",") != "telegram_1,telegram_2" {
		t.Fatalf("expected both profiles marked, got %v", store.marked)
	}
}

func TestQuietSearchNudgeRespectsOnlyProfile(t *testing.T) {
	store := &fakeQuietSearchStore{quiet: []profiles.QuietSearch{{ProfileID: "telegram_1", TelegramChatID: 1}, {ProfileID: "telegram_2", TelegramChatID: 2}}}
	var sent []int64
	nudger := quietSearchNudger{store: store, now: daytime, onlyProfileID: "telegram_2", send: func(ctx context.Context, chatID int64, text string) error {
		sent = append(sent, chatID)
		return nil
	}}

	nudger.NudgeQuietSearches(context.Background())

	if len(sent) != 1 || sent[0] != 2 || len(store.marked) != 1 {
		t.Fatalf("expected only telegram_2 nudged, got %v %v", sent, store.marked)
	}
}

func daytime() time.Time {
	return time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
}

func TestQuietSearchNudgeWaitsForDaytimeInGeorgia(t *testing.T) {
	store := &fakeQuietSearchStore{quiet: []profiles.QuietSearch{{ProfileID: "telegram_1", TelegramChatID: 1}}}
	sent := 0
	night := func() time.Time { return time.Date(2026, 11, 8, 19, 30, 0, 0, time.UTC) }
	nudger := quietSearchNudger{store: store, now: night, send: func(ctx context.Context, chatID int64, text string) error {
		sent++
		return nil
	}}

	nudger.NudgeQuietSearches(context.Background())

	if sent != 0 || len(store.marked) != 0 {
		t.Fatalf("expected no nudge at night, got %d sent and %v marked", sent, store.marked)
	}
}
