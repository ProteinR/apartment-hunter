package telegram

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/interview"
	"apartment-hunter/internal/preferences"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Bot struct {
	token        string
	outputDir    string
	profilesPath string
	client       *http.Client
	sessions     map[int64]*interview.Session
	mu           sync.Mutex
}

type Option func(*Bot)

func WithHTTPClient(client *http.Client) Option {
	return func(bot *Bot) {
		bot.client = client
	}
}

func WithProfilesPath(path string) Option {
	return func(bot *Bot) {
		bot.profilesPath = path
	}
}

func New(token, outputDir string, opts ...Option) *Bot {
	if outputDir == "" {
		outputDir = "preferences"
	}
	bot := &Bot{
		token:        token,
		outputDir:    outputDir,
		profilesPath: filepath.Join(outputDir, "profiles.json"),
		client:       &http.Client{Timeout: 20 * time.Second},
		sessions:     map[int64]*interview.Session{},
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
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/channels") {
		return b.listChannels(chatID)
	}
	if strings.HasPrefix(text, "/subscribe") {
		return b.subscribeChannel(chatID, text)
	}
	if strings.HasPrefix(text, "/unsubscribe") {
		return b.unsubscribeChannel(chatID, text)
	}
	if text == "/start" || text == "/preferences" || text == "/restart" {
		session := interview.NewSession()
		b.setSession(chatID, session)
		question, _ := session.CurrentQuestion()
		return "Starting preferences interview.\n" + question.Prompt, nil
	}

	session := b.getSession(chatID)
	if session == nil {
		return "Send /start to begin preferences interview.", nil
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
	if err := os.MkdirAll(b.outputDir, 0755); err != nil {
		return "", err
	}
	base := filepath.Join(b.outputDir, "preferences_"+strconv.FormatInt(chatID, 10))
	if err := preferences.Save(base+".yaml", prefs); err != nil {
		return "", err
	}
	if err := preferences.SaveJSON(base+".json", prefs); err != nil {
		return "", err
	}
	if err := b.saveProfile(chatID, base+".yaml"); err != nil {
		return "", err
	}
	return "Preferences saved:\n" + base + ".yaml\n" + base + ".json", nil
}

func (b *Bot) listChannels(chatID int64) (string, error) {
	cfg, err := profiles.Load(b.profilesPath)
	if err != nil {
		return "", err
	}
	profile, ok := cfg.Find(profiles.IDForTelegramChat(chatID))
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

func (b *Bot) subscribeChannel(chatID int64, text string) (string, error) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return "Usage: /subscribe @channel_username optional title", nil
	}
	username := strings.TrimPrefix(strings.TrimSpace(parts[1]), "@")
	if username == "" {
		return "Usage: /subscribe @channel_username optional title", nil
	}
	title := strings.TrimSpace(strings.TrimPrefix(text, parts[0]+" "+parts[1]))
	cfg, err := profiles.Load(b.profilesPath)
	if err != nil {
		return "", err
	}
	profileID := profiles.IDForTelegramChat(chatID)
	profile, ok := cfg.Find(profileID)
	if !ok {
		profile = profiles.Profile{ID: profileID, TelegramChatID: chatID}
		cfg.Upsert(profile)
	}
	added := cfg.UpsertChannel(profileID, profiles.ChannelSubscription{
		Source: sources.ChannelRef{
			ID:       username,
			Title:    title,
			Username: username,
		},
		Limit: 50,
	})
	if err := profiles.Save(b.profilesPath, cfg); err != nil {
		return "", err
	}
	if added {
		return "Subscribed to @" + username, nil
	}
	return "Updated subscription @" + username, nil
}

func (b *Bot) unsubscribeChannel(chatID int64, text string) (string, error) {
	parts := strings.Fields(text)
	if len(parts) != 2 {
		return "Usage: /unsubscribe @channel_username", nil
	}
	username := strings.TrimPrefix(strings.TrimSpace(parts[1]), "@")
	cfg, err := profiles.Load(b.profilesPath)
	if err != nil {
		return "", err
	}
	if !cfg.RemoveChannel(profiles.IDForTelegramChat(chatID), username) {
		return "No subscription found for @" + username, nil
	}
	if err := profiles.Save(b.profilesPath, cfg); err != nil {
		return "", err
	}
	return "Unsubscribed from @" + username, nil
}

func (b *Bot) saveProfile(chatID int64, preferencesPath string) error {
	cfg, err := profiles.Load(b.profilesPath)
	if err != nil {
		return err
	}
	cfg.Upsert(profiles.Profile{
		ID:              profiles.IDForTelegramChat(chatID),
		TelegramChatID:  chatID,
		PreferencesPath: preferencesPath,
	})
	return profiles.Save(b.profilesPath, cfg)
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
