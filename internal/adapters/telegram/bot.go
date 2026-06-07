package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Bot struct {
	token    string
	client   *http.Client
	sessions map[int64]*preferenceSession
	store    ProfileStore
	parser   preferenceparser.Service
	geometry preferenceparser.GeometryBuilder
	mu       sync.Mutex
}

type ProfileStore interface {
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	SaveSearchProfile(ctx context.Context, profile domain.UserSearchProfile) error
	UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error)
	RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error)
}

type preferenceSession struct {
	State string
	Draft preferenceparser.Draft
}

type Option func(*Bot)

func WithHTTPClient(client *http.Client) Option {
	return func(bot *Bot) {
		bot.client = client
	}
}

func WithPreferencesParser(parser preferenceparser.Service) Option {
	return func(bot *Bot) {
		bot.parser = parser
	}
}

func WithProfileStore(store ProfileStore) Option {
	return func(bot *Bot) {
		bot.store = store
	}
}

func WithGeometryBuilder(builder preferenceparser.GeometryBuilder) Option {
	return func(bot *Bot) {
		bot.geometry = builder
	}
}

func New(token, outputDir string, opts ...Option) *Bot {
	bot := &Bot{
		token:    token,
		client:   &http.Client{Timeout: 20 * time.Second},
		sessions: map[int64]*preferenceSession{},
	}
	for _, opt := range opts {
		opt(bot)
	}
	return bot
}

func (b *Bot) Run(ctxDone <-chan struct{}) error {
	if b.token == "" {
		return fmt.Errorf("telegram token is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-ctxDone:
			cancel()
		case <-ctx.Done():
		}
	}()

	api, err := telegrambot.New(
		b.token,
		telegrambot.WithHTTPClient(20*time.Second, b.client),
		telegrambot.WithDefaultHandler(b.handleUpdate),
	)
	if err != nil {
		return err
	}
	api.Start(ctx)
	return nil
}

func (b *Bot) HandleText(chatID int64, text string) (string, error) {
	return b.handleText(chatID, text)
}

func (b *Bot) handleUpdate(ctx context.Context, api *telegrambot.Bot, update *models.Update) {
	if update.Message == nil || update.Message.Text == "" {
		return
	}

	chatID := update.Message.Chat.ID
	if err := b.handleMessage(ctx, api, chatID, update.Message.Text); err != nil {
		_ = sendMessage(ctx, api, chatID, "Error: "+err.Error())
	}
}

func (b *Bot) handleMessage(ctx context.Context, api *telegrambot.Bot, chatID int64, text string) error {
	response, err := b.handleTextResponse(chatID, text)
	if err != nil {
		return err
	}
	return sendMessageWithMarkup(ctx, api, chatID, response.Text, response.ReplyMarkup)
}

func (b *Bot) handleText(chatID int64, text string) (string, error) {
	response, err := b.handleTextResponse(chatID, text)
	return response.Text, err
}

type textResponse struct {
	Text        string
	ReplyMarkup models.ReplyMarkup
}

func (b *Bot) handleTextResponse(chatID int64, text string) (textResponse, error) {
	if b.store == nil {
		return textResponse{}, fmt.Errorf("profile store is required")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/channels") {
		reply, err := b.listChannels(context.Background(), chatID)
		return textResponse{Text: reply}, err
	}
	if strings.HasPrefix(text, "/subscribe") {
		reply, err := b.subscribeChannel(context.Background(), chatID, text)
		return textResponse{Text: reply}, err
	}
	if strings.HasPrefix(text, "/unsubscribe") {
		reply, err := b.unsubscribeChannel(context.Background(), chatID, text)
		return textResponse{Text: reply}, err
	}
	if text == "/start" || text == "/preferences" || text == "/restart" {
		b.setSession(chatID, &preferenceSession{State: "awaiting_query"})
		return textResponse{Text: "Опиши квартиру одним сообщением."}, nil
	}

	session := b.getSession(chatID)
	if session == nil {
		return textResponse{Text: "Отправьте /preferences, чтобы настроить поиск квартиры."}, nil
	}
	switch session.State {
	case "awaiting_query":
		if b.parser.Normalizer == nil || b.parser.Extractor == nil {
			return textResponse{}, fmt.Errorf("preferences parser is required")
		}
		draft, err := b.parser.BuildDraft(context.Background(), text)
		if err != nil {
			return textResponse{}, err
		}
		session.State = "awaiting_confirmation"
		session.Draft = draft
		b.setSession(chatID, session)
		return textResponse{
			Text:        buildPreferencesPreview(draft),
			ReplyMarkup: confirmationKeyboard(),
		}, nil
	case "awaiting_confirmation":
		switch normalizeConfirmation(text) {
		case "save":
			b.deleteSession(chatID)
			if err := b.saveDraft(context.Background(), chatID, session.Draft); err != nil {
				return textResponse{}, err
			}
			return textResponse{Text: "Готово, фильтры сохранены.", ReplyMarkup: removeKeyboard()}, nil
		case "edit":
			session.State = "awaiting_query"
			b.setSession(chatID, session)
			return textResponse{Text: "Отправь исправленный запрос одним сообщением.", ReplyMarkup: removeKeyboard()}, nil
		case "cancel":
			b.deleteSession(chatID)
			return textResponse{Text: "Настройка отменена.", ReplyMarkup: removeKeyboard()}, nil
		default:
			return textResponse{Text: "Выбери Save, Edit или Cancel.", ReplyMarkup: confirmationKeyboard()}, nil
		}
	default:
		b.deleteSession(chatID)
		return textResponse{Text: "Отправьте /preferences, чтобы настроить поиск квартиры."}, nil
	}
}

func (b *Bot) listChannels(ctx context.Context, chatID int64) (string, error) {
	profile, ok, err := b.store.GetProfile(ctx, profiles.IDForTelegramChat(chatID))
	if err != nil {
		return "", err
	}
	if !ok {
		return "Send /start first to create your profile.", nil
	}
	if len(profile.Channels) == 0 {
		return "No channel subscriptions. Add one with /subscribe @channel_username optional title", nil
	}
	lines := []string{"Channel subscriptions:"}
	for _, sub := range profile.Channels {
		handle := sub.Source.Username
		if handle == "" {
			handle = sub.Source.ID
		}
		name := "@" + strings.TrimPrefix(handle, "@")
		if sub.Source.Title != "" {
			name += " - " + sub.Source.Title
		}
		lines = append(lines, "- "+name)
	}
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) subscribeChannel(ctx context.Context, chatID int64, text string) (string, error) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return "Usage: /subscribe @channel_username optional title", nil
	}
	username := strings.TrimPrefix(strings.TrimSpace(parts[1]), "@")
	if username == "" {
		return "Usage: /subscribe @channel_username optional title", nil
	}
	title := strings.TrimSpace(strings.TrimPrefix(text, parts[0]+" "+parts[1]))
	profileID := profiles.IDForTelegramChat(chatID)
	if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID}); err != nil {
		return "", err
	}
	added, err := b.store.UpsertProfileChannel(ctx, profileID, profiles.ChannelSubscription{
		Source: sources.ChannelRef{
			ID:       username,
			Title:    title,
			Username: username,
		},
		Limit: 50,
	})
	if err != nil {
		return "", err
	}
	if added {
		return "Subscribed to @" + username, nil
	}
	return "Updated subscription @" + username, nil
}

func (b *Bot) unsubscribeChannel(ctx context.Context, chatID int64, text string) (string, error) {
	parts := strings.Fields(text)
	if len(parts) != 2 {
		return "Usage: /unsubscribe @channel_username", nil
	}
	username := strings.TrimPrefix(strings.TrimSpace(parts[1]), "@")
	removed, err := b.store.RemoveProfileChannel(ctx, profiles.IDForTelegramChat(chatID), username)
	if err != nil {
		return "", err
	}
	if !removed {
		return "No subscription found for @" + username, nil
	}
	return "Unsubscribed from @" + username, nil
}

func (b *Bot) saveProfile(ctx context.Context, chatID int64, prefs domain.UserPreferences) error {
	return b.store.UpsertProfile(ctx, profiles.Profile{
		ID:             profiles.IDForTelegramChat(chatID),
		TelegramChatID: chatID,
		Preferences:    &prefs,
	})
}

func (b *Bot) saveDraft(ctx context.Context, chatID int64, draft preferenceparser.Draft) error {
	userID := profiles.IDForTelegramChat(chatID)
	var area *domain.GeoSearchArea
	if b.geometry != nil {
		built, err := b.geometry.BuildSearchGeometry(ctx, draft.Preferences.LocationPreferences)
		if err != nil {
			return err
		}
		area = built
		draft.Preferences.UnresolvedLocationRules = append(draft.Preferences.UnresolvedLocationRules, built.UnresolvedRules...)
	}
	if err := b.store.SaveSearchProfile(ctx, domain.UserSearchProfile{
		UserID:          userID,
		RawQuery:        draft.RawQuery,
		NormalizedQuery: draft.NormalizedQuery,
		Preferences:     draft.Preferences,
		SearchArea:      area,
	}); err != nil {
		return err
	}
	return b.saveProfile(ctx, chatID, draft.Preferences)
}

func (b *Bot) getSession(chatID int64) *preferenceSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[chatID]
}

func (b *Bot) setSession(chatID int64, session *preferenceSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sessions[chatID] = session
}

func (b *Bot) deleteSession(chatID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sessions, chatID)
}

func sendMessage(ctx context.Context, api *telegrambot.Bot, chatID int64, text string) error {
	return sendMessageWithMarkup(ctx, api, chatID, text, nil)
}

func sendMessageWithMarkup(ctx context.Context, api *telegrambot.Bot, chatID int64, text string, markup models.ReplyMarkup) error {
	_, err := api.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID:      chatID,
		Text:        text,
		ReplyMarkup: markup,
	})
	return err
}

func SendText(ctx context.Context, token string, chatID int64, text string) error {
	api, err := telegrambot.New(token)
	if err != nil {
		return err
	}
	return sendMessage(ctx, api, chatID, text)
}
