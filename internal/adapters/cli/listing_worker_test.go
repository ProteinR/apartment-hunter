package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"testing"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"
)

type fakeWorkerStore struct {
	profiles  map[string]profiles.Profile
	evaluated map[string]bool
}

func (s *fakeWorkerStore) ListSubscribedProfileIDs(ctx context.Context) ([]string, error) {
	var ids []string
	for id, profile := range s.profiles {
		if len(profile.Channels) > 0 {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *fakeWorkerStore) GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error) {
	profile, ok := s.profiles[id]
	return profile, ok, nil
}

func (s *fakeWorkerStore) EvaluationExists(ctx context.Context, profileID, channelID string, messageID int) (bool, error) {
	return s.evaluated[evaluationKey(profileID, channelID, messageID)], nil
}

func (s *fakeWorkerStore) UpdateProfileChannelSource(ctx context.Context, profileID string, current, resolved sources.ChannelRef) error {
	return nil
}

type fakeHistory struct {
	messages map[string][]sources.ChannelMessage
	failures map[string]error
	calls    int
}

func (h *fakeHistory) Fetch(ctx context.Context, channel sources.ChannelRef, cursor sources.Cursor, limit int) ([]sources.ChannelMessage, sources.Cursor, error) {
	h.calls++
	if err := h.failures[channel.ID]; err != nil {
		return nil, sources.Cursor{}, err
	}
	return h.messages[channel.ID], sources.Cursor{}, nil
}

func evaluationKey(profileID, channelID string, messageID int) string {
	return fmt.Sprintf("%s|%s|%d", profileID, channelID, messageID)
}

func subscribedProfile(id string, chatID int64, channels ...string) profiles.Profile {
	profile := profiles.Profile{ID: id, TelegramChatID: chatID, Preferences: &domain.UserPreferences{}}
	for _, channel := range channels {
		profile.Channels = append(profile.Channels, profiles.ChannelSubscription{Source: sources.ChannelRef{ID: channel, Username: channel}})
	}
	return profile
}

func newTestWorker(store *fakeWorkerStore, history *fakeHistory, processed *[]string) *listingWorker {
	return &listingWorker{
		store:   store,
		history: history,
		process: func(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage) error {
			*processed = append(*processed, profile.ID+"<-"+message.ChannelID)
			store.evaluated[evaluationKey(profile.ID, message.ChannelID, message.MessageID)] = true
			return nil
		},
		out:             io.Discard,
		errOut:          io.Discard,
		caughtUp:        map[string]bool{},
		catchUpFailures: map[string]catchUpFailure{},
	}
}

func TestListingWorkerDispatchesLiveMessagesToEachSubscribedProfile(t *testing.T) {
	store := &fakeWorkerStore{
		profiles: map[string]profiles.Profile{
			"telegram_1": subscribedProfile("telegram_1", 1, "batumi_rent", "shared_rent"),
			"telegram_2": subscribedProfile("telegram_2", 2, "tbilisi_rent", "shared_rent"),
			"telegram_3": {ID: "telegram_3", TelegramChatID: 3, Channels: subscribedProfile("telegram_3", 3, "tbilisi_rent").Channels},
		},
		evaluated: map[string]bool{},
	}
	var processed []string
	worker := newTestWorker(store, &fakeHistory{}, &processed)

	for _, message := range []sources.ChannelMessage{
		{ChannelID: "batumi_rent", MessageID: 1, Text: "Сдается квартира"},
		{ChannelID: "tbilisi_rent", MessageID: 2, Text: "Сдается квартира"},
		{ChannelID: "shared_rent", MessageID: 3, Text: "Сдается квартира"},
		{ChannelID: "shared_rent", MessageID: 3, Text: "Сдается квартира"},
		{ChannelID: "unknown_rent", MessageID: 4, Text: "Сдается квартира"},
	} {
		if err := worker.HandleLiveMessage(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}

	want := []string{"telegram_1<-batumi_rent", "telegram_2<-tbilisi_rent", "telegram_1<-shared_rent", "telegram_2<-shared_rent"}
	if len(processed) != len(want) {
		t.Fatalf("expected %v, got %v", want, processed)
	}
	for i := range want {
		if processed[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, processed)
		}
	}
}

func TestListingWorkerCatchesUpEachNewSubscriptionOnce(t *testing.T) {
	store := &fakeWorkerStore{
		profiles: map[string]profiles.Profile{
			"telegram_1": subscribedProfile("telegram_1", 1, "batumi_rent"),
		},
		evaluated: map[string]bool{},
	}
	history := &fakeHistory{messages: map[string][]sources.ChannelMessage{
		"batumi_rent":  {{ChannelID: "batumi_rent", MessageID: 2, Text: "Сдам квартиру"}, {ChannelID: "batumi_rent", MessageID: 1, Text: "Сдам квартиру"}},
		"tbilisi_rent": {{ChannelID: "tbilisi_rent", MessageID: 5, Text: "Сдам квартиру"}},
	}}
	var processed []string
	worker := newTestWorker(store, history, &processed)

	worker.CatchUpNewSubscriptions(context.Background())
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 1 || len(processed) != 2 {
		t.Fatalf("expected one fetch and two processed messages, got %d fetches and %v", history.calls, processed)
	}

	store.profiles["telegram_2"] = subscribedProfile("telegram_2", 2, "tbilisi_rent")
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 2 || len(processed) != 3 || processed[2] != "telegram_2<-tbilisi_rent" {
		t.Fatalf("expected new profile subscription to be caught up, got %d fetches and %v", history.calls, processed)
	}
}

func TestListingWorkerSkipsCatchUpUntilProfileHasPreferences(t *testing.T) {
	profile := subscribedProfile("telegram_2", 2, "tbilisi_rent")
	profile.Preferences = nil
	store := &fakeWorkerStore{profiles: map[string]profiles.Profile{"telegram_2": profile}, evaluated: map[string]bool{}}
	history := &fakeHistory{messages: map[string][]sources.ChannelMessage{
		"tbilisi_rent": {{ChannelID: "tbilisi_rent", MessageID: 5, Text: "Сдам квартиру"}},
	}}
	var processed []string
	worker := newTestWorker(store, history, &processed)

	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 0 {
		t.Fatalf("expected no fetch without preferences, got %d", history.calls)
	}
	profile.Preferences = &domain.UserPreferences{}
	store.profiles["telegram_2"] = profile
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 1 || len(processed) != 1 {
		t.Fatalf("expected catch-up after preferences were saved, got %d fetches and %v", history.calls, processed)
	}
}

func TestListingWorkerBacksOffFailingSubscriptionCatchUp(t *testing.T) {
	store := &fakeWorkerStore{
		profiles:  map[string]profiles.Profile{"telegram_2": subscribedProfile("telegram_2", 2, "private_rent")},
		evaluated: map[string]bool{},
	}
	history := &fakeHistory{failures: map[string]error{"private_rent": errors.New("CHANNEL_PRIVATE")}}
	var processed []string
	worker := newTestWorker(store, history, &processed)
	current := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	worker.now = func() time.Time { return current }

	worker.CatchUpNewSubscriptions(context.Background())
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 1 {
		t.Fatalf("expected retry to wait for backoff, got %d fetches", history.calls)
	}
	current = current.Add(subscriptionCatchUpInterval * 2)
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 2 {
		t.Fatalf("expected retry after backoff, got %d fetches", history.calls)
	}
	current = current.Add(subscriptionCatchUpInterval * 2)
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 2 {
		t.Fatalf("expected growing backoff after second failure, got %d fetches", history.calls)
	}
	current = current.Add(subscriptionCatchUpMaxBackoff)
	delete(history.failures, "private_rent")
	worker.CatchUpNewSubscriptions(context.Background())
	worker.CatchUpNewSubscriptions(context.Background())
	if history.calls != 3 {
		t.Fatalf("expected one successful catch-up after recovery, got %d fetches", history.calls)
	}
}
