package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"
)

const (
	subscriptionCatchUpInterval   = time.Minute
	subscriptionCatchUpMaxBackoff = 6 * time.Hour
)

type catchUpFailure struct {
	attempts    int
	nextAttempt time.Time
}

type listingWorkerStore interface {
	ListSubscribedProfileIDs(ctx context.Context) ([]string, error)
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	EvaluationExists(ctx context.Context, profileID, channelID string, messageID int) (bool, error)
	UpdateProfileChannelSource(ctx context.Context, profileID string, current, resolved sources.ChannelRef) error
}

type channelHistoryFetcher interface {
	Fetch(ctx context.Context, channel sources.ChannelRef, cursor sources.Cursor, limit int) ([]sources.ChannelMessage, sources.Cursor, error)
}

type profileMessageProcessor func(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage) error

type listingWorker struct {
	store         listingWorkerStore
	history       channelHistoryFetcher
	albumText     albumTextResolver
	process       profileMessageProcessor
	onlyProfileID string
	out           io.Writer
	errOut        io.Writer

	processing      sync.Mutex
	caughtUp        map[string]bool
	catchUpFailures map[string]catchUpFailure
	now             func() time.Time
}

func (w *listingWorker) profileIDs(ctx context.Context) ([]string, error) {
	if w.onlyProfileID != "" {
		return []string{w.onlyProfileID}, nil
	}
	return w.store.ListSubscribedProfileIDs(ctx)
}

func (w *listingWorker) HandleLiveMessage(ctx context.Context, message sources.ChannelMessage) error {
	profileIDs, err := w.profileIDs(ctx)
	if err != nil {
		return err
	}
	enriched := false
	for _, profileID := range profileIDs {
		profile, ok, err := w.subscribedProfile(ctx, profileID, message)
		if err != nil {
			w.logError("profile %s lookup failed: %v", profileID, err)
			continue
		}
		if !ok {
			continue
		}
		if !enriched {
			message = enrichGroupedAlbumText(ctx, w.albumText, message, w.errOut)
			enriched = true
		}
		if err := w.processForProfile(ctx, profile, message); err != nil {
			w.logError("processing %s/%d for %s failed: %v", message.ChannelID, message.MessageID, profileID, err)
		}
	}
	return nil
}

func (w *listingWorker) CatchUpNewSubscriptions(ctx context.Context) {
	profileIDs, err := w.profileIDs(ctx)
	if err != nil {
		w.logError("listing subscribed profiles failed: %v", err)
		return
	}
	for _, profileID := range profileIDs {
		profile, ok, err := w.store.GetProfile(ctx, profileID)
		if err != nil {
			w.logError("profile %s lookup failed: %v", profileID, err)
			continue
		}
		if !ok || profile.Preferences == nil {
			continue
		}
		for _, sub := range profile.Channels {
			if ctx.Err() != nil {
				return
			}
			key := catchUpKey(profileID, sub.Source)
			if w.caughtUp[key] {
				continue
			}
			failure, failedBefore := w.catchUpFailures[key]
			if failedBefore && w.currentTime().Before(failure.nextAttempt) {
				continue
			}
			resolved, err := w.catchUpSubscription(ctx, profileID, sub)
			if err != nil {
				failure.attempts++
				backoff := subscriptionCatchUpInterval << min(failure.attempts, 10)
				if backoff > subscriptionCatchUpMaxBackoff {
					backoff = subscriptionCatchUpMaxBackoff
				}
				failure.nextAttempt = w.currentTime().Add(backoff)
				w.catchUpFailures[key] = failure
				w.logError("catch-up of @%s for %s failed (attempt %d, next in %s): %v", subscriptionName(sub), profileID, failure.attempts, backoff, err)
				continue
			}
			delete(w.catchUpFailures, key)
			w.caughtUp[key] = true
			w.caughtUp[catchUpKey(profileID, resolved)] = true
		}
	}
}

func (w *listingWorker) RunSubscriptionCatchUp(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.CatchUpNewSubscriptions(ctx)
		}
	}
}

func (w *listingWorker) catchUpSubscription(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (sources.ChannelRef, error) {
	limit := sub.Limit
	if limit <= 0 {
		limit = 50
	}
	if w.out != nil {
		fmt.Fprintf(w.out, "Catching up Telegram history for @%s (%s), limit=%d\n", subscriptionName(sub), profileID, limit)
	}
	messages, _, err := w.history.Fetch(ctx, sub.Source, sources.Cursor{}, limit)
	if err != nil {
		return sub.Source, err
	}
	resolved := resolvedChannelSource(sub.Source, messages)
	if err := w.store.UpdateProfileChannelSource(ctx, profileID, sub.Source, resolved); err != nil {
		return sub.Source, err
	}
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if strings.TrimSpace(message.Text) == "" && len(message.Photos) == 0 {
			continue
		}
		profile, ok, err := w.subscribedProfile(ctx, profileID, message)
		if err != nil {
			return resolved, err
		}
		if !ok {
			continue
		}
		message = enrichGroupedAlbumText(ctx, w.albumText, message, w.errOut)
		if err := w.processForProfile(ctx, profile, message); err != nil {
			return resolved, err
		}
	}
	return resolved, nil
}

func (w *listingWorker) subscribedProfile(ctx context.Context, profileID string, message sources.ChannelMessage) (profiles.Profile, bool, error) {
	profile, ok, err := w.store.GetProfile(ctx, profileID)
	if err != nil || !ok {
		return profiles.Profile{}, false, err
	}
	if profile.Preferences == nil || !profileSubscribedToMessage(profile, message) {
		return profiles.Profile{}, false, nil
	}
	return profile, true, nil
}

func (w *listingWorker) processForProfile(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage) error {
	w.processing.Lock()
	defer w.processing.Unlock()
	if strings.TrimSpace(message.Text) == "" && len(message.Photos) == 0 {
		return nil
	}
	exists, err := w.store.EvaluationExists(ctx, profile.ID, message.ChannelID, message.MessageID)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return w.process(ctx, profile, message)
}

func (w *listingWorker) currentTime() time.Time {
	if w.now != nil {
		return w.now()
	}
	return time.Now()
}

func (w *listingWorker) logError(format string, args ...any) {
	if w.errOut != nil {
		fmt.Fprintf(w.errOut, format+"\n", args...)
	}
}

func catchUpKey(profileID string, source sources.ChannelRef) string {
	channel := normalizeTelegramChannelKey(source.ID)
	if channel == "" {
		channel = normalizeTelegramChannelKey(source.Username)
	}
	return profileID + "|" + channel
}
