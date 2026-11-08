package telegram

import (
	"context"
	"testing"

	"github.com/go-telegram/bot/models"
)

func listingRejectQuery(fromID, chatID int64, messageID int) *models.CallbackQuery {
	return &models.CallbackQuery{
		ID:   "q",
		From: models.User{ID: fromID},
		Data: ListingRejectCallbackData,
		Message: models.MaybeInaccessibleMessage{
			Type:    models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: messageID, Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate}},
		},
	}
}

func TestRejectListingFromButtonSavesFeedbackOnce(t *testing.T) {
	store := newFakeProfileStore()
	store.feedbackMatched = true
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, rejected, err := bot.rejectListingFromButton(context.Background(), 42, 55)
	if err != nil || !rejected || reply != "Отзыв сохранен: feedback #7" {
		t.Fatalf("unexpected first press: %q rejected=%v err=%v", reply, rejected, err)
	}
	got := store.feedbacks[0]
	if got.FeedbackMessageID != 55 || got.RepliedMessageID != 55 || got.Text != listingRejectFeedbackText {
		t.Fatalf("unexpected feedback: %+v", got)
	}

	reply, rejected, err = bot.rejectListingFromButton(context.Background(), 42, 55)
	if err != nil || !rejected || reply != listingRejectAlreadyText || len(store.feedbacks) != 1 {
		t.Fatalf("repeat press must not rerun feedback: %q rejected=%v feedbacks=%d", reply, rejected, len(store.feedbacks))
	}
}

func TestRejectListingFromButtonReportsUnknownNotification(t *testing.T) {
	store := newFakeProfileStore()
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	reply, rejected, err := bot.rejectListingFromButton(context.Background(), 42, 55)
	if err != nil || rejected || reply != listingRejectNotFoundText {
		t.Fatalf("unexpected reply for unknown notification: %q rejected=%v err=%v", reply, rejected, err)
	}
}

func TestListingRejectCallbackIgnoresForeignNotification(t *testing.T) {
	store := newFakeProfileStore()
	store.feedbackMatched = true
	bot := New("test-token", t.TempDir(), WithProfileStore(store))

	bot.handleCallback(context.Background(), nil, listingRejectQuery(7, 42, 55))
	if len(store.feedbacks) != 0 {
		t.Fatalf("foreign notification must not be rejected, got %+v", store.feedbacks)
	}
}

func TestListingRejectCallbackRequiresBotAccess(t *testing.T) {
	store := newFakeProfileStore()
	store.feedbackMatched = true
	bot := New("test-token", t.TempDir(), WithProfileStore(store), WithAllowedChats([]int64{1}))

	bot.handleCallback(context.Background(), nil, listingRejectQuery(42, 42, 55))
	if len(store.feedbacks) != 0 {
		t.Fatalf("user without access must not reach feedback, got %+v", store.feedbacks)
	}

	allowed := New("test-token", t.TempDir(), WithProfileStore(store), WithAllowedChats([]int64{42}))
	allowed.handleCallback(context.Background(), nil, listingRejectQuery(42, 42, 55))
	if len(store.feedbacks) != 1 {
		t.Fatalf("allowed user press should save feedback, got %+v", store.feedbacks)
	}
}
