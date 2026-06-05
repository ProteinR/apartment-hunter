package telegram

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
	"apartment-hunter/internal/interview"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Bot struct {
	token    string
	client   *http.Client
	sessions map[int64]*interview.Session
	resolver geocoding.Resolver
	store    ProfileStore
	mu       sync.Mutex
}

type ProfileStore interface {
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error)
	RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error)
}

type Option func(*Bot)

func WithHTTPClient(client *http.Client) Option {
	return func(bot *Bot) {
		bot.client = client
	}
}

func WithLocationResolver(resolver geocoding.Resolver) Option {
	return func(bot *Bot) {
		bot.resolver = resolver
	}
}

func WithProfileStore(store ProfileStore) Option {
	return func(bot *Bot) {
		bot.store = store
	}
}

func New(token, outputDir string, opts ...Option) *Bot {
	bot := &Bot{
		token:    token,
		client:   &http.Client{Timeout: 20 * time.Second},
		sessions: map[int64]*interview.Session{},
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
	reply, err := b.handleText(chatID, text)
	if err != nil {
		return err
	}
	return sendMessage(ctx, api, chatID, reply)
}

func (b *Bot) handleText(chatID int64, text string) (string, error) {
	if b.store == nil {
		return "", fmt.Errorf("profile store is required")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/channels") {
		return b.listChannels(context.Background(), chatID)
	}
	if strings.HasPrefix(text, "/subscribe") {
		return b.subscribeChannel(context.Background(), chatID, text)
	}
	if strings.HasPrefix(text, "/unsubscribe") {
		return b.unsubscribeChannel(context.Background(), chatID, text)
	}
	if text == "/start" || text == "/preferences" || text == "/restart" {
		session := interview.NewSessionWithResolver(b.resolver)
		b.setSession(chatID, session)
		question, _ := session.CurrentQuestion()
		return "Настроим поиск квартиры. Отвечайте коротко, можно на русском.\n\n" + question.Prompt, nil
	}

	session := b.getSession(chatID)
	if session == nil {
		return "Отправьте /start, чтобы настроить поиск квартиры.", nil
	}

	next, done, err := session.Answer(text)
	if err != nil {
		current, _ := session.CurrentQuestion()
		return err.Error() + "\n" + current.Prompt, nil
	}
	if !done {
		return next.Prompt, nil
	}

	prefs := session.Preferences()
	b.deleteSession(chatID)
	if err := b.saveProfile(context.Background(), chatID, prefs); err != nil {
		return "", err
	}
	return "Готово, предпочтения сохранены.", nil
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

func (b *Bot) getSession(chatID int64) *interview.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[chatID]
}

func (b *Bot) setSession(chatID int64, session *interview.Session) {
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
	_, err := api.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
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
