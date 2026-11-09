package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"apartment-hunter/internal/access"

	"github.com/go-telegram/bot/models"
)

type fakeAccessStore struct {
	entries []access.Entry
}

func (s *fakeAccessStore) FindAccess(ctx context.Context, chatID int64, username string) (access.Entry, bool, error) {
	username = access.NormalizeUsername(username)
	best := -1
	for i, entry := range s.entries {
		matches := (chatID == 0 && username != "" && entry.Username == username) ||
			(chatID != 0 && (entry.TelegramChatID == chatID || (username != "" && entry.TelegramChatID == 0 && entry.Username == username)))
		if !matches {
			continue
		}
		if best < 0 || (entry.Status == access.StatusAllowed && s.entries[best].Status != access.StatusAllowed) {
			best = i
		}
	}
	if best < 0 {
		return access.Entry{}, false, nil
	}
	return s.entries[best], true, nil
}

func (s *fakeAccessStore) SaveAccess(ctx context.Context, entry access.Entry) (bool, error) {
	entry.Username = access.NormalizeUsername(entry.Username)
	index := -1
	for i, existing := range s.entries {
		if (entry.TelegramChatID != 0 && existing.TelegramChatID == entry.TelegramChatID) || (entry.TelegramChatID == 0 && existing.Username == entry.Username) {
			index = i
		}
	}
	if entry.TelegramChatID != 0 && entry.Username != "" {
		var kept []access.Entry
		for i, existing := range s.entries {
			if existing.TelegramChatID == 0 && existing.Username == entry.Username {
				if i < index {
					index--
				}
				continue
			}
			if i != index && existing.Username == entry.Username {
				existing.Username = ""
			}
			kept = append(kept, existing)
		}
		if index >= len(kept) {
			index = -1
		}
		s.entries = kept
	}
	if index < 0 {
		s.entries = append(s.entries, entry)
		return true, nil
	}
	existing := s.entries[index]
	if entry.Username != "" {
		existing.Username = entry.Username
	}
	if entry.DisplayName != "" {
		existing.DisplayName = entry.DisplayName
	}
	existing.Status = entry.Status
	s.entries[index] = existing
	return false, nil
}

func (s *fakeAccessStore) ListAccess(ctx context.Context) ([]access.Entry, error) {
	return s.entries, nil
}

type sentMessage struct {
	chatID int64
	text   string
	markup models.ReplyMarkup
}

func newAccessBot(t *testing.T, accessStore *fakeAccessStore) (*Bot, *[]sentMessage) {
	t.Helper()
	var sent []sentMessage
	bot := New("test-token", t.TempDir(),
		WithProfileStore(newFakeProfileStore()),
		WithAllowedChats([]int64{100}),
		WithAllowedUsernames([]string{"@env_friend"}),
		WithAdminChats([]int64{1}),
		WithAccessStore(accessStore),
	)
	bot.notifier = func(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) error {
		sent = append(sent, sentMessage{chatID: chatID, text: text, markup: markup})
		return nil
	}
	return bot, &sent
}

func messageFrom(chatID int64, username, text string) *models.Message {
	return &models.Message{Chat: models.Chat{ID: chatID, Type: models.ChatTypePrivate}, From: &models.User{ID: chatID, Username: username, FirstName: "Анна"}, Text: text}
}

func TestStrangerRequestsAccessOnceAndAdminGetsButtons(t *testing.T) {
	store := &fakeAccessStore{}
	bot, sent := newAccessBot(t, store)

	if bot.authorizeMessage(context.Background(), messageFrom(200, "anna", "/start")) {
		t.Fatal("expected a stranger to be refused")
	}
	if len(store.entries) != 1 || store.entries[0].Status != access.StatusPending || store.entries[0].Username != "anna" {
		t.Fatalf("expected a pending entry, got %+v", store.entries)
	}
	if len(*sent) != 2 || (*sent)[0].chatID != 200 || (*sent)[0].text != accessRequestSentText {
		t.Fatalf("expected the stranger to be told about the request, got %+v", *sent)
	}
	adminMessage := (*sent)[1]
	keyboard, ok := adminMessage.markup.(*models.InlineKeyboardMarkup)
	if adminMessage.chatID != 1 || !ok || keyboard.InlineKeyboard[0][0].CallbackData != "access:allow:200" || !strings.Contains(adminMessage.text, "Анна, @anna, id 200") {
		t.Fatalf("expected an admin request with buttons, got %+v", adminMessage)
	}

	bot.authorizeMessage(context.Background(), messageFrom(200, "anna", "ау"))
	if len(*sent) != 2 || len(store.entries) != 1 {
		t.Fatalf("expected no duplicate request and no immediate reminder, got %+v", *sent)
	}
	bot.accessThrottle.reminderEvery = time.Nanosecond
	bot.authorizeMessage(context.Background(), messageFrom(200, "anna", "ау"))
	if len(*sent) != 3 || (*sent)[2].text != accessPendingText {
		t.Fatalf("expected a pending reminder after the interval, got %+v", *sent)
	}
}

func TestAdminAllowsRequestAndUserIsGreeted(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 200, Username: "anna", Status: access.StatusPending}}}
	bot, sent := newAccessBot(t, store)

	if answer := bot.handleAccessCallback(context.Background(), 1, "access:allow:200"); answer != "✅ Доступ открыт" {
		t.Fatalf("unexpected answer %q", answer)
	}
	if store.entries[0].Status != access.StatusAllowed {
		t.Fatalf("expected access allowed, got %+v", store.entries[0])
	}
	if len(*sent) != 1 || (*sent)[0].chatID != 200 || !strings.HasPrefix((*sent)[0].text, accessGrantedPrefix) || !strings.Contains((*sent)[0].text, newUserGreeting) {
		t.Fatalf("expected the user to get the onboarding greeting, got %+v", *sent)
	}
	if !bot.authorizeMessage(context.Background(), messageFrom(200, "anna", "Тбилиси")) {
		t.Fatal("expected the allowed user to pass")
	}
}

func TestOnlyAdminCanDecideRequests(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 200, Status: access.StatusPending}}}
	bot, _ := newAccessBot(t, store)

	if answer := bot.handleAccessCallback(context.Background(), 100, "access:allow:200"); answer != "Решать запросы может только администратор." {
		t.Fatalf("unexpected answer %q", answer)
	}
	if store.entries[0].Status != access.StatusPending {
		t.Fatalf("expected the request untouched, got %+v", store.entries[0])
	}
}

func TestDeniedUserIsToldOnceThenIgnored(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 200, Status: access.StatusPending}}}
	bot, sent := newAccessBot(t, store)

	bot.handleAccessCallback(context.Background(), 1, "access:deny:200")
	if len(*sent) != 1 || (*sent)[0].text != accessDeniedText {
		t.Fatalf("expected a denial notice, got %+v", *sent)
	}
	if bot.authorizeMessage(context.Background(), messageFrom(200, "", "привет")) || len(*sent) != 1 {
		t.Fatalf("expected later messages to be ignored silently, got %+v", *sent)
	}
}

func TestGrantByUsernameLetsUserInAndRecordsChatID(t *testing.T) {
	store := &fakeAccessStore{}
	bot, _ := newAccessBot(t, store)

	reply, err := bot.GrantBotAccess(context.Background(), "@Boris")
	if err != nil || !strings.Contains(reply, "пусть он напишет боту /start") {
		t.Fatalf("unexpected grant reply %q %v", reply, err)
	}
	if !bot.authorizeMessage(context.Background(), messageFrom(300, "boris", "/start")) {
		t.Fatal("expected the pre-granted user to pass")
	}
	if store.entries[0].TelegramChatID != 300 || store.entries[0].Status != access.StatusAllowed {
		t.Fatalf("expected the chat id to be recorded, got %+v", store.entries)
	}
}

func TestRevokeRefusesEnvironmentUsersAndAdmins(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 300, Username: "boris", Status: access.StatusAllowed}}}
	bot, _ := newAccessBot(t, store)

	if reply, _ := bot.RevokeBotAccess(context.Background(), "@env_friend"); !strings.Contains(reply, "только там") {
		t.Fatalf("expected an environment notice, got %q", reply)
	}
	if reply, _ := bot.RevokeBotAccess(context.Background(), "1"); !strings.HasPrefix(reply, "error: нельзя") {
		t.Fatalf("expected admin revoke to be refused, got %q", reply)
	}
	if reply, _ := bot.RevokeBotAccess(context.Background(), "boris"); !strings.Contains(reply, "закрыт") || store.entries[0].Status != access.StatusDenied {
		t.Fatalf("expected boris to be revoked, got %q %+v", reply, store.entries)
	}
	if bot.authorizeMessage(context.Background(), messageFrom(300, "boris", "привет")) {
		t.Fatal("expected the revoked user to be refused")
	}
}

func TestEnvironmentUsersAndAdminsPassWithoutStore(t *testing.T) {
	bot, _ := newAccessBot(t, &fakeAccessStore{})
	for _, message := range []*models.Message{messageFrom(100, "", "x"), messageFrom(555, "Env_Friend", "x"), messageFrom(1, "", "x")} {
		if !bot.authorizeMessage(context.Background(), message) {
			t.Fatalf("expected %d to pass", message.Chat.ID)
		}
	}
}

func TestStrangerWithoutAdminsGetsClosedNotice(t *testing.T) {
	store := &fakeAccessStore{}
	var sent []sentMessage
	bot := New("test-token", t.TempDir(), WithProfileStore(newFakeProfileStore()), WithAllowedChats([]int64{100}), WithAccessStore(store))
	bot.notifier = func(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) error {
		sent = append(sent, sentMessage{chatID: chatID, text: text})
		return nil
	}

	bot.authorizeMessage(context.Background(), messageFrom(200, "anna", "/start"))
	if len(store.entries) != 0 || len(sent) != 1 || sent[0].text != accessClosedText {
		t.Fatalf("expected a closed notice without a request, got %+v %+v", store.entries, sent)
	}
}

func TestOnlyConfiguredChatsAreAdmins(t *testing.T) {
	bot := New("test-token", t.TempDir(), WithAdminChats([]int64{42, 0, 42}))
	if !bot.IsAdmin(context.Background(), 42) || bot.IsAdmin(context.Background(), 43) || bot.IsAdmin(context.Background(), 0) || len(bot.adminChats) != 1 {
		t.Fatalf("expected only chat 42 to be an admin, got %v", bot.adminChats)
	}
}

func TestReusedUsernameDoesNotInheritBoundGrant(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 200, Username: "anna", Status: access.StatusAllowed}}}
	bot, _ := newAccessBot(t, store)

	if bot.authorizeMessage(context.Background(), messageFrom(201, "anna", "/start")) {
		t.Fatal("expected a new chat with a reused username to be refused")
	}
	if !bot.authorizeMessage(context.Background(), messageFrom(200, "anna2", "привет")) {
		t.Fatal("expected the original chat to keep access after renaming")
	}
	for _, entry := range store.entries {
		if entry.TelegramChatID == 201 && entry.Status != access.StatusPending {
			t.Fatalf("expected the impostor to stay pending, got %+v", store.entries)
		}
	}
}

func TestPendingUserGrantedByUsernameGetsIn(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 200, Status: access.StatusPending}, {Username: "anna", Status: access.StatusAllowed}}}
	bot, _ := newAccessBot(t, store)

	if !bot.authorizeMessage(context.Background(), messageFrom(200, "anna", "привет")) {
		t.Fatal("expected the username grant to win over the pending chat row")
	}
	if len(store.entries) != 1 || store.entries[0].TelegramChatID != 200 || store.entries[0].Status != access.StatusAllowed {
		t.Fatalf("expected rows merged into one allowed chat row, got %+v", store.entries)
	}
}

func TestDecidedRequestCannotBeOverriddenByStaleButton(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 200, Status: access.StatusAllowed}}}
	bot, _ := newAccessBot(t, store)

	if answer := bot.handleAccessCallback(context.Background(), 1, "access:deny:200"); !strings.HasPrefix(answer, "Уже решено") || store.entries[0].Status != access.StatusAllowed {
		t.Fatalf("expected a stale button to be ignored, got %q %+v", answer, store.entries)
	}
}

func TestAdminRequestNotificationsAreCapped(t *testing.T) {
	store := &fakeAccessStore{}
	bot, sent := newAccessBot(t, store)

	for i := 0; i < accessRequestsPerHour+3; i++ {
		bot.authorizeMessage(context.Background(), messageFrom(int64(1000+i), "", "/start"))
	}
	adminMessages := 0
	for _, message := range *sent {
		if message.chatID == 1 {
			adminMessages++
		}
	}
	if adminMessages != accessRequestsPerHour || len(store.entries) != accessRequestsPerHour+3 {
		t.Fatalf("expected %d admin pings and all requests stored, got %d pings and %d entries", accessRequestsPerHour, adminMessages, len(store.entries))
	}
}

func TestGroupChatMessagesAreIgnored(t *testing.T) {
	if isPrivateChatMessage(&models.Message{Chat: models.Chat{ID: -5, Type: models.ChatTypeGroup}, From: &models.User{ID: 100}}) {
		t.Fatal("expected group messages to be ignored")
	}
	if isPrivateChatMessage(&models.Message{Chat: models.Chat{ID: 100, Type: models.ChatTypePrivate}, From: &models.User{ID: 101}}) {
		t.Fatal("expected a private chat with a different sender to be ignored")
	}
	if !isPrivateChatMessage(messageFrom(100, "", "x")) {
		t.Fatal("expected a private message to pass")
	}
}

func TestAdminToolsReportOpenMode(t *testing.T) {
	bot := New("test-token", t.TempDir(), WithAccessStore(&fakeAccessStore{}))
	if reply, _ := bot.GrantBotAccess(context.Background(), "@anna"); reply != accessOpenModeText {
		t.Fatalf("expected the open-mode notice, got %q", reply)
	}
}

func TestGrantByUsernameOfAdminDoesNotLockAdminChat(t *testing.T) {
	store := &fakeAccessStore{entries: []access.Entry{{TelegramChatID: 1, Username: "owner", Status: access.StatusPending}}}
	bot, _ := newAccessBot(t, store)
	lock := bot.chatLock(1)
	lock.Lock()
	defer lock.Unlock()

	done := make(chan string, 1)
	go func() {
		reply, _ := bot.GrantBotAccess(context.Background(), "@owner")
		done <- reply
	}()
	select {
	case reply := <-done:
		if !strings.Contains(reply, "уже есть доступ") {
			t.Fatalf("expected the admin notice, got %q", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected granting an admin's username not to wait on the admin chat lock")
	}
}
