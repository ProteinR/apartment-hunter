package telegram

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/access"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	accessCallbackPrefix   = "access:"
	accessAllowAction      = "allow"
	accessDenyAction       = "deny"
	accessReminderInterval = time.Hour
	accessRequestWindow    = time.Hour
	accessRequestsPerHour  = 10
	accessRequestSentText  = "Привет! Этот бот ищет квартиры в аренду и работает по приглашению. Я отправил администратору запрос на доступ — как только он ответит, напишу сюда."
	accessPendingText      = "Запрос на доступ уже у администратора. Как только он ответит, я напишу."
	accessClosedText       = "Этот бот ищет квартиры в аренду и работает по приглашению. Попроси владельца бота открыть тебе доступ."
	accessGrantedPrefix    = "Администратор открыл тебе доступ.\n\n"
	accessDeniedText       = "Администратор пока не открыл доступ к боту."
	accessOpenModeText     = "Ограничений доступа сейчас нет: ботом может пользоваться любой. Чтобы включить доступ по приглашению, задай TELEGRAM_ALLOWED_CHAT_IDS или TELEGRAM_ALLOWED_USERNAMES на сервере."
	accessSaveFailedText   = "Не получилось сохранить решение, попробуй еще раз."
)

type AccessStore interface {
	FindAccess(ctx context.Context, telegramChatID int64, username string) (access.Entry, bool, error)
	SaveAccess(ctx context.Context, entry access.Entry) (bool, error)
	ListAccess(ctx context.Context) ([]access.Entry, error)
}

type notifier func(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) error

type accessThrottle struct {
	mu            sync.Mutex
	lastReminder  map[int64]time.Time
	requestTimes  []time.Time
	reminderEvery time.Duration
}

func WithAccessStore(store AccessStore) Option {
	return func(bot *Bot) {
		bot.accessStore = store
	}
}

func WithAdminChats(ids []int64) Option {
	return func(bot *Bot) {
		for _, id := range ids {
			if id != 0 && !slices.Contains(bot.adminChats, id) {
				bot.adminChats = append(bot.adminChats, id)
			}
		}
	}
}

func (t *accessThrottle) allowReminder(chatID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastReminder == nil {
		t.lastReminder = map[int64]time.Time{}
	}
	every := t.reminderEvery
	if every == 0 {
		every = accessReminderInterval
	}
	if last, ok := t.lastReminder[chatID]; ok && time.Since(last) < every {
		return false
	}
	for id, last := range t.lastReminder {
		if time.Since(last) >= every {
			delete(t.lastReminder, id)
		}
	}
	t.lastReminder[chatID] = time.Now()
	return true
}

func (t *accessThrottle) allowAdminRequest() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := time.Now().Add(-accessRequestWindow)
	t.requestTimes = slices.DeleteFunc(t.requestTimes, func(at time.Time) bool { return at.Before(cutoff) })
	if len(t.requestTimes) >= accessRequestsPerHour {
		return false
	}
	t.requestTimes = append(t.requestTimes, time.Now())
	return true
}

func (b *Bot) IsAdmin(ctx context.Context, chatID int64) bool {
	return chatID != 0 && slices.Contains(b.adminChats, chatID)
}

func (b *Bot) restrictedAccess() bool {
	return len(b.allowedChats) > 0 || len(b.allowedUsernames) > 0
}

func (b *Bot) allowedByEnvironment(chatID int64, username string) bool {
	if chatID != 0 && slices.Contains(b.allowedChats, chatID) {
		return true
	}
	username = access.NormalizeUsername(username)
	if username == "" {
		return false
	}
	for _, allowed := range b.allowedUsernames {
		if access.NormalizeUsername(allowed) == username {
			return true
		}
	}
	return false
}

func senderDisplayName(user *models.User) string {
	if user == nil {
		return ""
	}
	return strings.TrimSpace(user.FirstName + " " + user.LastName)
}

func isPrivateChatMessage(message *models.Message) bool {
	return message.Chat.Type == models.ChatTypePrivate && message.From != nil && message.From.ID == message.Chat.ID
}

func (b *Bot) authorizeMessage(ctx context.Context, message *models.Message) bool {
	chatID := message.Chat.ID
	username := ""
	if message.From != nil {
		username = message.From.Username
	}
	if !b.restrictedAccess() || b.allowedByEnvironment(chatID, username) || b.IsAdmin(ctx, chatID) {
		return true
	}
	if b.accessStore == nil {
		return false
	}
	entry, found, err := b.accessStore.FindAccess(ctx, chatID, username)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telegram bot access lookup failed for chat %d: %v\n", chatID, err)
		return false
	}
	if found && entry.Status == access.StatusAllowed {
		if entry.TelegramChatID != chatID || entry.Username != access.NormalizeUsername(username) {
			if _, err := b.accessStore.SaveAccess(ctx, access.Entry{TelegramChatID: chatID, Username: username, DisplayName: senderDisplayName(message.From), Status: access.StatusAllowed}); err != nil {
				fmt.Fprintf(os.Stderr, "telegram bot access backfill failed for chat %d: %v\n", chatID, err)
			}
		}
		return true
	}
	if found && entry.Status == access.StatusDenied {
		return false
	}
	if found {
		if b.accessThrottle.allowReminder(chatID) {
			b.notify(ctx, chatID, accessPendingText, removeKeyboard())
		}
		return false
	}
	b.requestAccess(ctx, access.Entry{TelegramChatID: chatID, Username: username, DisplayName: senderDisplayName(message.From), Status: access.StatusPending})
	return false
}

func (b *Bot) requestAccess(ctx context.Context, entry access.Entry) {
	admins := b.adminChats
	if len(admins) == 0 {
		if b.accessThrottle.allowReminder(entry.TelegramChatID) {
			b.notify(ctx, entry.TelegramChatID, accessClosedText, removeKeyboard())
		}
		return
	}
	created, err := b.accessStore.SaveAccess(ctx, entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telegram bot access request failed for chat %d: %v\n", entry.TelegramChatID, err)
		return
	}
	if !created {
		return
	}
	b.accessThrottle.allowReminder(entry.TelegramChatID)
	b.notify(ctx, entry.TelegramChatID, accessRequestSentText, removeKeyboard())
	if !b.accessThrottle.allowAdminRequest() {
		return
	}
	chatID := strconv.FormatInt(entry.TelegramChatID, 10)
	markup := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
		{Text: "Разрешить", CallbackData: accessCallbackPrefix + accessAllowAction + ":" + chatID},
		{Text: "Отклонить", CallbackData: accessCallbackPrefix + accessDenyAction + ":" + chatID},
	}}}
	for _, admin := range admins {
		b.notify(ctx, admin, accessRequestAdminText(entry), markup)
	}
}

func accessRequestAdminText(entry access.Entry) string {
	return "Новый запрос доступа к боту: " + entry.Label() + ".\n\nРазрешить пользоваться ботом?"
}

func (b *Bot) notify(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) {
	if b.notifier != nil {
		_ = b.notifier(ctx, chatID, text, markup)
	}
}

func (b *Bot) handleCallback(ctx context.Context, api *telegrambot.Bot, query *models.CallbackQuery) {
	if query.Data == ListingRejectCallbackData {
		b.handleListingRejectCallback(ctx, api, query)
		return
	}
	if !b.IsAdmin(ctx, query.From.ID) {
		if api != nil {
			_, _ = api.AnswerCallbackQuery(ctx, &telegrambot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Решать запросы может только администратор."})
		}
		return
	}
	if api != nil {
		_, _ = api.AnswerCallbackQuery(ctx, &telegrambot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	}
	answer := b.handleAccessCallback(ctx, query.From.ID, query.Data)
	if api == nil || answer == "" {
		return
	}
	if message := query.Message.Message; message != nil {
		_, _ = api.EditMessageText(ctx, &telegrambot.EditMessageTextParams{ChatID: message.Chat.ID, MessageID: message.ID, Text: message.Text + "\n\n" + answer})
	}
}

func (b *Bot) handleAccessCallback(ctx context.Context, adminChatID int64, data string) string {
	parts := strings.Split(strings.TrimPrefix(data, accessCallbackPrefix), ":")
	if !strings.HasPrefix(data, accessCallbackPrefix) || len(parts) != 2 {
		return ""
	}
	if !b.IsAdmin(ctx, adminChatID) || b.accessStore == nil {
		return "Решать запросы может только администратор."
	}
	chatID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || chatID == 0 {
		return ""
	}
	entry, found, err := b.accessStore.FindAccess(ctx, chatID, "")
	if err != nil || !found || entry.TelegramChatID != chatID {
		return "Запрос не найден."
	}
	if entry.Status != access.StatusPending {
		return "Уже решено: " + accessStatusLabel(entry.Status) + ". Изменить решение можно сообщением боту."
	}
	switch parts[0] {
	case accessAllowAction:
		if err := b.allow(ctx, entry); err != nil {
			fmt.Fprintf(os.Stderr, "telegram bot access allow failed for chat %d: %v\n", chatID, err)
			return accessSaveFailedText
		}
		return "✅ Доступ открыт"
	case accessDenyAction:
		entry.Status = access.StatusDenied
		if _, err := b.accessStore.SaveAccess(ctx, entry); err != nil {
			fmt.Fprintf(os.Stderr, "telegram bot access deny failed for chat %d: %v\n", chatID, err)
			return accessSaveFailedText
		}
		b.notify(ctx, entry.TelegramChatID, accessDeniedText, removeKeyboard())
		return "⛔ Запрос отклонен"
	}
	return ""
}

func (b *Bot) allow(ctx context.Context, entry access.Entry) error {
	wasAllowed := entry.Status == access.StatusAllowed
	entry.Status = access.StatusAllowed
	if entry.TelegramChatID == 0 {
		_, err := b.accessStore.SaveAccess(ctx, entry)
		return err
	}
	lock := b.chatLock(entry.TelegramChatID)
	lock.Lock()
	defer lock.Unlock()
	if _, err := b.accessStore.SaveAccess(ctx, entry); err != nil {
		return err
	}
	if wasAllowed {
		return nil
	}
	response, err := b.startConversation(ctx, entry.TelegramChatID)
	if err != nil {
		return err
	}
	b.notify(ctx, entry.TelegramChatID, accessGrantedPrefix+response.Text, removeKeyboard())
	return nil
}

func parseUserRef(ref string) (int64, string, bool) {
	ref = strings.TrimSpace(ref)
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil && id > 0 {
		return id, "", true
	}
	if username, ok := parsePublicTelegramUsername(ref); ok && username != "" {
		return 0, access.NormalizeUsername(username), true
	}
	if username, ok := cleanTelegramUsername(ref); ok && username != "" {
		return 0, access.NormalizeUsername(username), true
	}
	return 0, "", false
}

func (b *Bot) ListBotUsers(ctx context.Context) (string, error) {
	if !b.restrictedAccess() {
		return accessOpenModeText, nil
	}
	var lines []string
	for _, id := range b.adminChats {
		lines = append(lines, fmt.Sprintf("- id %d: администратор", id))
	}
	for _, id := range b.allowedChats {
		lines = append(lines, fmt.Sprintf("- id %d: доступ из настроек сервера (закрыть можно только там)", id))
	}
	for _, username := range b.allowedUsernames {
		lines = append(lines, "- @"+access.NormalizeUsername(username)+": доступ из настроек сервера (закрыть можно только там)")
	}
	if b.accessStore != nil {
		entries, err := b.accessStore.ListAccess(ctx)
		if err != nil {
			return "", err
		}
		for _, entry := range entries {
			lines = append(lines, "- "+entry.Label()+": "+accessStatusLabel(entry.Status))
		}
	}
	return strings.Join(lines, "\n"), nil
}

func accessStatusLabel(status access.Status) string {
	switch status {
	case access.StatusAllowed:
		return "доступ открыт"
	case access.StatusPending:
		return "ждет решения"
	case access.StatusDenied:
		return "доступ закрыт"
	}
	return string(status)
}

func (b *Bot) GrantBotAccess(ctx context.Context, ref string) (string, error) {
	if !b.restrictedAccess() {
		return accessOpenModeText, nil
	}
	if b.accessStore == nil {
		return "error: access list is not configured", nil
	}
	chatID, username, ok := parseUserRef(ref)
	if !ok {
		return "error: нужен @username или числовой Telegram id", nil
	}
	if b.IsAdmin(ctx, chatID) || b.allowedByEnvironment(chatID, username) {
		return "У " + ref + " уже есть доступ: администратор или настройки сервера.", nil
	}
	entry, found, err := b.accessStore.FindAccess(ctx, chatID, username)
	if err != nil {
		return "", err
	}
	if !found || (chatID != 0 && entry.TelegramChatID != chatID) {
		entry = access.Entry{TelegramChatID: chatID, Username: username}
	}
	if b.IsAdmin(ctx, entry.TelegramChatID) {
		return "У " + ref + " уже есть доступ: администратор или настройки сервера.", nil
	}
	if err := b.allow(ctx, entry); err != nil {
		return "", err
	}
	if entry.TelegramChatID == 0 {
		return "Доступ для " + entry.Label() + " открыт. Бот еще не знает этого пользователя: пусть он напишет боту /start, и сразу начнется настройка.", nil
	}
	return "Доступ для " + entry.Label() + " открыт, пользователь получил приветствие.", nil
}

func (b *Bot) RevokeBotAccess(ctx context.Context, ref string) (string, error) {
	if !b.restrictedAccess() {
		return accessOpenModeText, nil
	}
	if b.accessStore == nil {
		return "error: access list is not configured", nil
	}
	chatID, username, ok := parseUserRef(ref)
	if !ok {
		return "error: нужен @username или числовой Telegram id", nil
	}
	if b.IsAdmin(ctx, chatID) {
		return "error: нельзя закрыть доступ администратору", nil
	}
	if b.allowedByEnvironment(chatID, username) {
		return "Доступ " + ref + " задан в настройках сервера (TELEGRAM_ALLOWED_CHAT_IDS или TELEGRAM_ALLOWED_USERNAMES), закрыть его можно только там.", nil
	}
	entry, found, err := b.accessStore.FindAccess(ctx, chatID, username)
	if err != nil {
		return "", err
	}
	if !found || (chatID != 0 && entry.TelegramChatID != chatID) {
		return "Пользователь " + ref + " не найден в списке доступа.", nil
	}
	entry.Status = access.StatusDenied
	if _, err := b.accessStore.SaveAccess(ctx, entry); err != nil {
		return "", err
	}
	return "Доступ для " + entry.Label() + " закрыт.", nil
}
