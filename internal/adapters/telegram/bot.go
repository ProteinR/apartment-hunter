package telegram

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/adapters/mcpagent"
	"apartment-hunter/internal/cities"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Bot struct {
	token            string
	client           *http.Client
	sessions         map[int64]*preferenceSession
	store            ProfileStore
	parser           preferenceparser.Service
	geometry         preferenceparser.GeometryBuilder
	mapRenderer      googlemaps.StaticMapRenderer
	mcpAgent         *mcpagent.Agent
	mu               sync.Mutex
	allowedChats     []int64
	allowedUsernames []string
	defaultCity      cities.City
}

type ProfileStore interface {
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	SaveSearchProfile(ctx context.Context, profile domain.UserSearchProfile) error
	UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error)
	RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error)
	SaveListingFeedback(ctx context.Context, feedback domain.ListingFeedback) (string, *domain.ApartmentListing, error)
}

type preferenceSession struct {
	State      string
	Draft      preferenceparser.Draft
	SearchArea *domain.GeoSearchArea
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

func WithMCPAgent(agent *mcpagent.Agent) Option {
	return func(bot *Bot) {
		bot.mcpAgent = agent
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

func WithSearchAreaMapRenderer(renderer googlemaps.StaticMapRenderer) Option {
	return func(bot *Bot) {
		bot.mapRenderer = renderer
	}
}

func WithAllowedChats(ids []int64) Option {
	return func(bot *Bot) {
		bot.allowedChats = ids
	}
}

func WithAllowedUsernames(usernames []string) Option {
	return func(bot *Bot) {
		bot.allowedUsernames = usernames
	}
}

func New(token, outputDir string, opts ...Option) *Bot {
	bot := &Bot{
		token:       token,
		client:      &http.Client{Timeout: 20 * time.Second},
		sessions:    map[int64]*preferenceSession{},
		defaultCity: cities.Batumi,
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
	isAllowed := false
	if len(b.allowedChats) == 0 && len(b.allowedUsernames) == 0 {
		isAllowed = true
	} else {
		for _, id := range b.allowedChats {
			if id == chatID {
				isAllowed = true
				break
			}
		}
		if !isAllowed && update.Message.From != nil && update.Message.From.Username != "" {
			senderUsername := strings.TrimPrefix(update.Message.From.Username, "@")
			for _, username := range b.allowedUsernames {
				cleaned := strings.TrimPrefix(username, "@")
				if strings.EqualFold(cleaned, senderUsername) {
					isAllowed = true
					break
				}
			}
		}
	}
	if !isAllowed {
		return
	}
	if update.Message.ReplyToMessage != nil && b.getSession(chatID) == nil && !strings.HasPrefix(strings.TrimSpace(update.Message.Text), "/") {
		reply, handled, err := b.saveReplyFeedback(ctx, update.Message)
		if err != nil {
			_ = sendMessage(ctx, api, chatID, "Error: "+err.Error())
			return
		}
		if handled {
			_ = sendMessageWithReply(ctx, api, chatID, update.Message.ID, reply)
			return
		}
	}
	if err := b.handleMessage(ctx, api, chatID, update.Message.Text); err != nil {
		_ = sendMessage(ctx, api, chatID, "Error: "+err.Error())
	}
}

func (b *Bot) handleMessage(ctx context.Context, api *telegrambot.Bot, chatID int64, text string) error {
	response, err := b.handleTextResponse(chatID, text)
	if err != nil {
		return err
	}
	if len(response.DocumentBytes) > 0 {
		return sendDocumentBytesWithMarkup(ctx, api, chatID, response.DocumentFilename, response.DocumentBytes, response.Text, response.ReplyMarkup)
	}
	if len(response.PhotoBytes) > 0 {
		textRunes := []rune(response.Text)
		if len(textRunes) <= 1024 {
			return sendPhotoBytesWithMarkup(ctx, api, chatID, response.PhotoFilename, response.PhotoBytes, response.Text, response.ReplyMarkup)
		}
		if err := sendPhotoBytesWithMarkup(ctx, api, chatID, response.PhotoFilename, response.PhotoBytes, "Карта зоны поиска", nil); err != nil {
			return err
		}
		return sendMessageWithMarkup(ctx, api, chatID, response.Text, response.ReplyMarkup)
	}
	return sendMessageWithMarkup(ctx, api, chatID, response.Text, response.ReplyMarkup)
}

func (b *Bot) handleText(chatID int64, text string) (string, error) {
	response, err := b.handleTextResponse(chatID, text)
	return response.Text, err
}

type textResponse struct {
	Text             string
	ReplyMarkup      models.ReplyMarkup
	PhotoFilename    string
	PhotoBytes       []byte
	DocumentFilename string
	DocumentBytes    []byte
}

func (b *Bot) handleTextResponse(chatID int64, text string) (textResponse, error) {
	if b.store == nil {
		return textResponse{}, fmt.Errorf("profile store is required")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/channels") {
		reply, err := b.listChannels(context.Background(), chatID)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if strings.HasPrefix(text, "/subscribe") {
		reply, err := b.subscribeChannel(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if strings.HasPrefix(text, "/unsubscribe") {
		reply, err := b.unsubscribeChannel(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if strings.HasPrefix(text, "/debug") {
		reply, err := b.debugMode(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if text == "/city" || strings.HasPrefix(text, "/city ") {
		reply, err := b.cityCommand(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if strings.HasPrefix(text, "/mcp") {
		reply, err := b.mcpMode(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if text == "/clear" || text == "/reset" {
		profileID := profiles.IDForTelegramChat(chatID)
		b.deleteSession(chatID)
		profile, ok, err := b.store.GetProfile(context.Background(), profileID)
		if err == nil && ok {
			profile.MCPHistory = []profiles.MCPMessage{}
			if err := b.store.UpsertProfile(context.Background(), profile); err != nil {
				return textResponse{}, err
			}
		}
		return textResponse{
			Text:        "Контекст общения сброшен. Сохраненные фильтры остались без изменений.",
			ReplyMarkup: mainCommandKeyboard(),
		}, nil
	}
	if looksLikeChannelSubscription(text) {
		reply, err := b.subscribeChannelsFromText(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if text == "/start" || text == "/preferences" || text == "/restart" {
		b.setSession(chatID, &preferenceSession{State: "awaiting_query"})
		return textResponse{Text: preferencesStartMessage, ReplyMarkup: mainCommandKeyboard()}, nil
	}

	profile, ok, err := b.store.GetProfile(context.Background(), profiles.IDForTelegramChat(chatID))
	if err == nil && ok && profiles.MCPModeEnabled(profile) {
		if b.mcpAgent == nil {
			return textResponse{Text: "Агент MCP не настроен."}, nil
		}
		reply, prefsChanged, areaChanged, err := b.mcpAgent.ProcessCommand(context.Background(), profile.ID, text)
		if err != nil {
			return textResponse{Text: "Ошибка агента: " + err.Error()}, nil
		}
		if prefsChanged != nil || areaChanged != nil {
			var draft preferenceparser.Draft
			if prefsChanged != nil {
				normalized := preferenceparser.NormalizePreferences(*prefsChanged)
				draft.Preferences = normalized
			} else if profile.Preferences != nil {
				draft.Preferences = *profile.Preferences
			}
			if err := b.saveDraft(context.Background(), chatID, draft, areaChanged); err != nil {
				return textResponse{Text: "Ошибка сохранения новых фильтров: " + err.Error()}, nil
			}
		}
		resp := textResponse{
			Text:        reply,
			ReplyMarkup: mainCommandKeyboard(),
		}
		if shouldAttachMCPMap(text, reply, prefsChanged != nil || areaChanged != nil) {
			if saStore, ok := b.store.(interface {
				LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error)
			}); ok {
				if geoJSON, ok, err := saStore.LatestSearchAreaGeoJSON(context.Background(), profile.ID); err == nil && ok {
					if mapImage, err := b.mapRenderer.SearchAreaImage(context.Background(), geoJSON); err == nil && len(mapImage) > 0 {
						resp.PhotoFilename = "search-area-map.png"
						resp.PhotoBytes = mapImage
					}
				}
			}
		}
		return resp, nil
	}

	session := b.getSession(chatID)
	if session == nil {
		return textResponse{Text: "Выберите команду кнопкой ниже или отправьте /preferences, чтобы настроить поиск квартиры.", ReplyMarkup: mainCommandKeyboard()}, nil
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
		draft.Preferences.City = b.draftCity(context.Background(), chatID, text, draft.Preferences.City).Key
		searchArea, err := b.prepareDraftGeometry(context.Background(), &draft)
		if err != nil {
			return textResponse{}, err
		}
		session.State = "awaiting_confirmation"
		session.Draft = draft
		session.SearchArea = searchArea
		b.setSession(chatID, session)
		return b.preferencesPreviewResponse(context.Background(), draft, searchArea), nil
	case "awaiting_confirmation":
		if looksLikeSearchAreaGeoJSON(text) {
			area, err := searchAreaFromGeoJSON(text)
			if err != nil {
				return textResponse{Text: "Не смог разобрать GeoJSON: " + err.Error() + "\n\n" + searchAreaEditInstructions(b.cityOf(session.Draft.Preferences)), ReplyMarkup: confirmationKeyboard()}, nil
			}
			session.SearchArea = area
			b.setSession(chatID, session)
			return b.preferencesPreviewResponse(context.Background(), session.Draft, session.SearchArea), nil
		}
		switch normalizeConfirmation(text) {
		case "save":
			b.deleteSession(chatID)
			if err := b.saveDraft(context.Background(), chatID, session.Draft, session.SearchArea); err != nil {
				return textResponse{}, err
			}
			reply, err := b.afterPreferencesSavedMessage(context.Background(), chatID)
			return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
		case "edit":
			session.State = "awaiting_query"
			b.setSession(chatID, session)
			return textResponse{Text: "Отправь исправленный запрос одним сообщением.", ReplyMarkup: mainCommandKeyboard()}, nil
		case "edit_area":
			return searchAreaEditResponse(session.SearchArea, b.cityOf(session.Draft.Preferences)), nil
		case "cancel":
			b.deleteSession(chatID)
			return textResponse{Text: "Настройка отменена.", ReplyMarkup: mainCommandKeyboard()}, nil
		default:
			return textResponse{Text: "Выбери Save, Edit, Edit area или Cancel. Можно также отправить GeoJSON прямо сюда одним сообщением.", ReplyMarkup: confirmationKeyboard()}, nil
		}
	default:
		b.deleteSession(chatID)
		return textResponse{Text: "Выберите команду кнопкой ниже или отправьте /preferences, чтобы настроить поиск квартиры.", ReplyMarkup: mainCommandKeyboard()}, nil
	}
}

func (b *Bot) afterPreferencesSavedMessage(ctx context.Context, chatID int64) (string, error) {
	channels, err := b.channelList(ctx, chatID)
	if err != nil {
		return "", err
	}
	var lines []string
	lines = append(lines, "Готово, фильтры сохранены.")
	lines = append(lines, "")
	lines = append(lines, "Теперь нужны источники объявлений. Я не вступаю в чаты автоматически: вступи в подходящие Telegram-чаты/группы вручную со своего аккаунта, а сюда пришли public ссылки или usernames.")
	lines = append(lines, "")
	if len(channels) > 0 {
		lines = append(lines, "Уже подключены:")
		lines = append(lines, channels...)
		lines = append(lines, "")
		lines = append(lines, "Можно добавить еще: пришли t.me/name или @name, по одному или несколько сразу.")
	} else {
		lines = append(lines, "Пока источников нет. Пришли, например:")
		lines = append(lines, "https://t.me/rent_channel")
		lines = append(lines, "@another_rent_channel")
	}
	lines = append(lines, "")
	lines = append(lines, "Посмотреть список: /channels")
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) listChannels(ctx context.Context, chatID int64) (string, error) {
	channels, err := b.channelList(ctx, chatID)
	if err != nil {
		return "", err
	}
	if len(channels) == 0 {
		return "Источники пока не подключены.\n\nВступи вручную в подходящие Telegram-чаты/группы, затем пришли сюда public ссылки или usernames, например:\nhttps://t.me/rent_channel\n@another_rent_channel\n\nАвтоматически вступать в чаты я не буду.", nil
	}
	lines := []string{"Подключенные источники:"}
	lines = append(lines, channels...)
	lines = append(lines, "", "Добавить еще: пришли t.me/name или @name.", "Удалить: /unsubscribe @name")
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) channelList(ctx context.Context, chatID int64) ([]string, error) {
	profile, ok, err := b.store.GetProfile(ctx, profiles.IDForTelegramChat(chatID))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	if len(profile.Channels) == 0 {
		return nil, nil
	}
	var lines []string
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
	return lines, nil
}

func shouldAttachMCPMap(text, reply string, filtersChanged bool) bool {
	if filtersChanged {
		return true
	}
	return textMentionsSearchAreaMap(text) || textMentionsSearchAreaMap(reply) || textMentionsSearchAreaSettings(text) || textMentionsSearchAreaSettings(reply)
}

func textMentionsSearchAreaSettings(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "фильтр") ||
		strings.Contains(lower, "настройк") ||
		strings.Contains(lower, "локаци")
}

func textMentionsSearchAreaMap(text string) bool {
	lower := strings.ToLower(text)
	if !(strings.Contains(lower, "зон") || strings.Contains(lower, "област") || strings.Contains(lower, "гео") || strings.Contains(lower, "search area")) {
		return false
	}
	return strings.Contains(lower, "карт") ||
		strings.Contains(lower, "изображ") ||
		strings.Contains(lower, "картин") ||
		strings.Contains(lower, "фото") ||
		strings.Contains(lower, "скрин") ||
		strings.Contains(lower, "покаж") ||
		strings.Contains(lower, "пришли") ||
		strings.Contains(lower, "отправ")
}

func (b *Bot) subscribeChannel(ctx context.Context, chatID int64, text string) (string, error) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return "Пришли public ссылку или username: /subscribe https://t.me/channel_name optional title", nil
	}
	username, ok := parsePublicTelegramUsername(parts[1])
	if !ok {
		return unsupportedChannelRefMessage(parts[1]), nil
	}
	if username == "" {
		return "Пришли public ссылку или username: /subscribe https://t.me/channel_name optional title", nil
	}
	title := strings.TrimSpace(strings.TrimPrefix(text, parts[0]+" "+parts[1]))
	return b.upsertChannel(ctx, chatID, username, title)
}

func (b *Bot) subscribeChannelsFromText(ctx context.Context, chatID int64, text string) (string, error) {
	refs := publicTelegramUsernamesFromText(text)
	if len(refs) == 0 {
		return unsupportedChannelRefMessage(text), nil
	}
	var added []string
	for _, username := range refs {
		reply, err := b.upsertChannel(ctx, chatID, username, "")
		if err != nil {
			return "", err
		}
		added = append(added, reply)
	}
	lines := append([]string{"Источники обновлены:"}, added...)
	lines = append(lines, "", "Важно: я не вступаю в чаты автоматически. Убедись, что аккаунт, которым запущен worker, уже вступил в эти чаты/группы.", "Список источников: /channels")
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) upsertChannel(ctx context.Context, chatID int64, username, title string) (string, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
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
		return "Добавлен @" + username, nil
	}
	return "Обновлен @" + username, nil
}

func (b *Bot) unsubscribeChannel(ctx context.Context, chatID int64, text string) (string, error) {
	parts := strings.Fields(text)
	if len(parts) != 2 {
		return "Использование: /unsubscribe @channel_username", nil
	}
	username, ok := parsePublicTelegramUsername(parts[1])
	if !ok {
		return unsupportedChannelRefMessage(parts[1]), nil
	}
	removed, err := b.store.RemoveProfileChannel(ctx, profiles.IDForTelegramChat(chatID), username)
	if err != nil {
		return "", err
	}
	if !removed {
		return "Источник @" + username + " не найден.", nil
	}
	return "Источник @" + username + " удален.", nil
}

func (b *Bot) debugMode(ctx context.Context, chatID int64, text string) (string, error) {
	parts := strings.Fields(strings.TrimSpace(text))
	profileID := profiles.IDForTelegramChat(chatID)

	if len(parts) == 1 {
		profile, ok, err := b.store.GetProfile(ctx, profileID)
		if err != nil {
			return "", err
		}
		enabled := true
		if ok {
			enabled = !profiles.DebugModeEnabled(profile)
		} else {
			enabled = false // default is true, so toggle is false
		}
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, DebugMode: &enabled}); err != nil {
			return "", err
		}
		return debugModeStatusMessage(enabled), nil
	}

	if strings.EqualFold(parts[1], "status") {
		profile, ok, err := b.store.GetProfile(ctx, profileID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "Debug mode: on\n\nВ этом режиме я пересылаю отчеты по обработанным объявлениям, кроме сообщений, которые не похожи на объявления о квартире. Выключить: /debug off", nil
		}
		return debugModeStatusMessage(profiles.DebugModeEnabled(profile)), nil
	}
	switch strings.ToLower(parts[1]) {
	case "on", "вкл", "enable", "enabled":
		enabled := true
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, DebugMode: &enabled}); err != nil {
			return "", err
		}
		return debugModeStatusMessage(true), nil
	case "off", "выкл", "disable", "disabled":
		enabled := false
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, DebugMode: &enabled}); err != nil {
			return "", err
		}
		return debugModeStatusMessage(false), nil
	default:
		return "Использование: /debug (переключение), /debug on или /debug off", nil
	}
}

func (b *Bot) mcpMode(ctx context.Context, chatID int64, text string) (string, error) {
	parts := strings.Fields(strings.TrimSpace(text))
	profileID := profiles.IDForTelegramChat(chatID)

	if len(parts) == 1 {
		profile, ok, err := b.store.GetProfile(ctx, profileID)
		if err != nil {
			return "", err
		}
		enabled := true
		if ok {
			enabled = !profiles.MCPModeEnabled(profile)
		}
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, MCPMode: &enabled}); err != nil {
			return "", err
		}
		return mcpModeStatusMessage(enabled), nil
	}

	if strings.EqualFold(parts[1], "status") {
		profile, ok, err := b.store.GetProfile(ctx, profileID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "MCP mode: off", nil
		}
		return mcpModeStatusMessage(profiles.MCPModeEnabled(profile)), nil
	}
	switch strings.ToLower(parts[1]) {
	case "on", "вкл", "enable", "enabled":
		enabled := true
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, MCPMode: &enabled}); err != nil {
			return "", err
		}
		return mcpModeStatusMessage(true), nil
	case "off", "выкл", "disable", "disabled":
		enabled := false
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, MCPMode: &enabled}); err != nil {
			return "", err
		}
		return mcpModeStatusMessage(false), nil
	case "clear", "сброс", "reset":
		if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profileID, TelegramChatID: chatID, MCPHistory: []profiles.MCPMessage{}}); err != nil {
			return "", err
		}
		return "Контекст агента успешно сброшен. Начинаем с чистого листа!", nil
	default:
		return "Использование: /mcp (переключение), /mcp on, /mcp off или /mcp clear", nil
	}
}

func (b *Bot) saveReplyFeedback(ctx context.Context, message *models.Message) (string, bool, error) {
	if b.store == nil {
		return "", false, fmt.Errorf("profile store is required")
	}
	if message.ReplyToMessage == nil {
		return "Ответь на сообщение с отчетом по квартире и напиши отзыв.", false, nil
	}
	text := strings.TrimSpace(message.Text)
	if text == "" {
		return "Пустой отзыв не сохранен.", false, nil
	}
	id, listing, err := b.store.SaveListingFeedback(ctx, domain.ListingFeedback{
		TelegramChatID:    message.Chat.ID,
		FeedbackMessageID: message.ID,
		RepliedMessageID:  message.ReplyToMessage.ID,
		Text:              text,
	})
	if err != nil {
		return "", false, err
	}
	if listing == nil {
		return "", false, nil
	}

	profile, profileOk, profileErr := b.store.GetProfile(ctx, profiles.IDForTelegramChat(message.Chat.ID))
	if profileErr == nil && profileOk && profiles.MCPModeEnabled(profile) && b.mcpAgent != nil {
		reply, prefsChanged, areaChanged, agentErr := b.mcpAgent.ProcessFeedback(ctx, profile.ID, *listing, text, message.Chat.ID, message.ReplyToMessage.ID)
		if agentErr != nil {
			return "Отзыв сохранен, но ошибка агента: " + agentErr.Error(), true, nil
		}
		if prefsChanged != nil || areaChanged != nil {
			var draft preferenceparser.Draft
			if prefsChanged != nil {
				draft.Preferences = *prefsChanged
			} else if profile.Preferences != nil {
				draft.Preferences = *profile.Preferences
			}
			if draftErr := b.saveDraft(ctx, message.Chat.ID, draft, areaChanged); draftErr != nil {
				return "Отзыв сохранен, но ошибка применения фильтров: " + draftErr.Error(), true, nil
			}
		}
		return reply, true, nil
	}

	return "Отзыв сохранен: feedback #" + id, true, nil
}

func debugModeStatusMessage(enabled bool) string {
	if enabled {
		return "Debug mode: on\n\nЯ пересылаю отчеты по обработанным объявлениям: прошедшим, отклоненным, дубликатам или ошибкам. Сообщения со статусом «не объявление о квартире» в чат не отправляю. Выключить: /debug off"
	}
	return "Debug mode: off\n\nЯ пересылаю только объявления, которые прошли фильтры. Включить все отчеты: /debug on"
}

func mcpModeStatusMessage(enabled bool) string {
	if enabled {
		return "MCP mode: on\n\nТеперь я работаю в режиме гибкого агента. Напиши мне текстовую команду (например, 'Пришли мне GeoJSON зоны'), и я постараюсь ее выполнить."
	}
	return "MCP mode: off\n\nРежим агента выключен. Текстовые сообщения снова воспринимаются как фильтры для поиска квартиры."
}

func looksLikeChannelSubscription(text string) bool {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "@") {
		return true
	}
	lower := strings.ToLower(text)
	return strings.Contains(lower, "t.me/") || strings.Contains(lower, "telegram.me/")
}

func publicTelegramUsernamesFromText(text string) []string {
	seen := map[string]bool{}
	var usernames []string
	for _, token := range strings.Fields(text) {
		username, ok := parsePublicTelegramUsername(token)
		if !ok || seen[strings.ToLower(username)] {
			continue
		}
		seen[strings.ToLower(username)] = true
		usernames = append(usernames, username)
	}
	return usernames
}

func parsePublicTelegramUsername(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	value = strings.Trim(value, " \t\r\n,.;:()[]{}<>")
	if value == "" {
		return "", false
	}
	if strings.HasPrefix(value, "@") {
		username := strings.TrimPrefix(value, "@")
		return cleanTelegramUsername(username)
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "t.me/") || strings.HasPrefix(lower, "telegram.me/") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Host), "www.")
	if host != "t.me" && host != "telegram.me" {
		return "", false
	}
	path := strings.Trim(parsed.Path, "/")
	if path == "" {
		return "", false
	}
	segments := strings.Split(path, "/")
	segment := segments[0]
	if strings.EqualFold(segment, "s") && len(segments) > 1 {
		segment = segments[1]
	}
	return cleanTelegramUsername(segment)
}

func cleanTelegramUsername(username string) (string, bool) {
	username = strings.TrimSpace(username)
	username = strings.Trim(username, " \t\r\n,.;:()[]{}<>")
	if username == "" ||
		strings.HasPrefix(username, "+") ||
		strings.EqualFold(username, "joinchat") ||
		strings.EqualFold(username, "c") {
		return "", false
	}
	if strings.Contains(username, "/") || strings.Contains(username, "?") || strings.Contains(username, "#") {
		return "", false
	}
	return strings.TrimPrefix(username, "@"), true
}

func unsupportedChannelRefMessage(value string) string {
	return "Пока я могу подключать только public Telegram-источники по @username или t.me/name.\n\nЕсли это invite-ссылка вида t.me/+..., вступи вручную, но для чтения мне все равно нужен public username канала/группы. Автоматическое вступление не выполняю."
}

func (b *Bot) prepareDraftGeometry(ctx context.Context, draft *preferenceparser.Draft) (*domain.GeoSearchArea, error) {
	if b.geometry == nil || draft == nil {
		return nil, nil
	}
	area, err := b.geometry.BuildSearchGeometry(ctx, b.cityOf(draft.Preferences), draft.Preferences.LocationPreferences)
	if err != nil {
		return nil, err
	}
	if area != nil {
		draft.Preferences.UnresolvedLocationRules = append(draft.Preferences.UnresolvedLocationRules, area.UnresolvedRules...)
		if strings.TrimSpace(area.GeoJSON) != "" {
			draft.Preferences.HasSearchArea = true
		}
	}
	return area, nil
}

func (b *Bot) preferencesPreviewResponse(ctx context.Context, draft preferenceparser.Draft, area *domain.GeoSearchArea) textResponse {
	response := textResponse{Text: buildPreferencesPreview(draft), ReplyMarkup: confirmationKeyboard()}
	if area == nil || strings.TrimSpace(area.GeoJSON) == "" {
		return response
	}
	mapImage, err := b.mapRenderer.SearchAreaImage(ctx, area.GeoJSON)
	if err != nil || len(mapImage) == 0 {
		return response
	}
	response.Text = buildPreferencesMapCaption(draft)
	response.PhotoFilename = "search-area-map.png"
	response.PhotoBytes = mapImage
	return response
}

func searchAreaEditResponse(area *domain.GeoSearchArea, city cities.City) textResponse {
	editorURL := city.GeoJSONEditorURL()
	if area == nil || strings.TrimSpace(area.GeoJSON) == "" {
		return textResponse{
			Text:        "Текущая зона пока не построена автоматически.\n\nОткрой geojson.io, нарисуй Polygon/MultiPolygon, скопируй весь GeoJSON из правой панели JSON и отправь его сюда одним сообщением.\n\n" + editorURL,
			ReplyMarkup: confirmationKeyboard(),
		}
	}
	geoJSON := prettyGeoJSON(area.GeoJSON)
	header := "Текущий GeoJSON зоны:\n\n"
	footer := "\n\nОткрой geojson.io, вставь этот JSON в правую панель JSON, поправь зону, затем скопируй весь обновленный GeoJSON и отправь его сюда одним сообщением.\n\n" + editorURL
	text := header + "```json\n" + geoJSON + "\n```" + footer
	if len([]rune(text)) <= 3900 {
		return textResponse{Text: text, ReplyMarkup: confirmationKeyboard()}
	}
	return textResponse{
		Text:             "Текущий GeoJSON зоны во вложении search-area.geojson.\n\nОткрой geojson.io, загрузи файл через Open > File или скопируй содержимое файла в правую панель JSON, поправь зону, затем отправь обновленный GeoJSON сюда одним сообщением.\n\n" + editorURL,
		ReplyMarkup:      confirmationKeyboard(),
		DocumentFilename: "search-area.geojson",
		DocumentBytes:    []byte(geoJSON),
	}
}

func (b *Bot) saveProfile(ctx context.Context, chatID int64, prefs domain.UserPreferences) error {
	return b.store.UpsertProfile(ctx, profiles.Profile{
		ID:             profiles.IDForTelegramChat(chatID),
		TelegramChatID: chatID,
		Preferences:    &prefs,
	})
}

func (b *Bot) saveDraft(ctx context.Context, chatID int64, draft preferenceparser.Draft, area *domain.GeoSearchArea) error {
	userID := profiles.IDForTelegramChat(chatID)
	if _, ok := cities.Find(draft.Preferences.City); !ok {
		draft.Preferences.City = b.profileCity(ctx, chatID).Key
	}
	if area == nil && b.geometry != nil {
		built, err := b.prepareDraftGeometry(ctx, &draft)
		if err != nil {
			return err
		}
		area = built
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

func (b *Bot) cityOf(prefs domain.UserPreferences) cities.City {
	return cities.ForKey(prefs.City, b.defaultCity)
}

func (b *Bot) profileCity(ctx context.Context, chatID int64) cities.City {
	profile, ok, err := b.store.GetProfile(ctx, profiles.IDForTelegramChat(chatID))
	if err != nil || !ok || profile.Preferences == nil {
		return b.defaultCity
	}
	return b.cityOf(*profile.Preferences)
}

func (b *Bot) draftCity(ctx context.Context, chatID int64, rawQuery, parsedCity string) cities.City {
	if city, ok := cities.LastMentioned(rawQuery); ok {
		return city
	}
	if city, ok := cities.Find(parsedCity); ok {
		return city
	}
	return b.profileCity(ctx, chatID)
}

func (b *Bot) cityCommand(ctx context.Context, chatID int64, text string) (string, error) {
	requested := strings.TrimSpace(strings.TrimPrefix(text, "/city"))
	current := b.profileCity(ctx, chatID)
	if requested == "" {
		return fmt.Sprintf("Город поиска: %s.\n\nСменить: /city tbilisi или /city batumi (доступны: %s).", current.DisplayName, cities.SupportedDisplayNames()), nil
	}
	city, ok := cities.Find(requested)
	if !ok {
		return fmt.Sprintf("Не знаю город %q. Доступны: %s.", requested, cities.SupportedDisplayNames()), nil
	}
	profile, found, err := b.store.GetProfile(ctx, profiles.IDForTelegramChat(chatID))
	if err != nil {
		return "", err
	}
	if !found || profile.Preferences == nil {
		return fmt.Sprintf("Фильтров пока нет. Отправь /preferences и укажи в запросе город: %s.", city.DisplayName), nil
	}
	if city.Key == current.Key {
		return fmt.Sprintf("Город поиска уже %s.", city.DisplayName), nil
	}
	b.deleteSession(chatID)
	prefs := *profile.Preferences
	prefs.City = city.Key
	prefs.LocationPreferences = nil
	prefs.Locations = nil
	prefs.UnresolvedLocationRules = nil
	prefs.HasSearchArea = false
	draft := preferenceparser.Draft{RawQuery: "/city " + city.Key, Preferences: prefs}
	if err := b.saveDraft(ctx, chatID, draft, &domain.GeoSearchArea{SRID: 4326}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Город поиска: %s. Районы и зона поиска от прошлого города сброшены, сейчас подходят объявления из любого района. Чтобы задать районы, отправь /preferences с описанием поиска.", city.DisplayName), nil
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
		Text:        markdownToHTML(text),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: markup,
	})
	if err != nil {
		_, err = api.SendMessage(ctx, &telegrambot.SendMessageParams{
			ChatID:      chatID,
			Text:        text,
			ReplyMarkup: markup,
		})
	}
	return err
}

func sendPhotoBytesWithMarkup(ctx context.Context, api *telegrambot.Bot, chatID int64, filename string, data []byte, caption string, markup models.ReplyMarkup) error {
	if strings.TrimSpace(filename) == "" {
		filename = "image.png"
	}
	_, err := api.SendPhoto(ctx, &telegrambot.SendPhotoParams{
		ChatID: chatID,
		Photo: &models.InputFileUpload{
			Filename: filename,
			Data:     bytes.NewReader(data),
		},
		Caption:     markdownToHTML(caption),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: markup,
	})
	if err != nil {
		_, err = api.SendPhoto(ctx, &telegrambot.SendPhotoParams{
			ChatID: chatID,
			Photo: &models.InputFileUpload{
				Filename: filename,
				Data:     bytes.NewReader(data),
			},
			Caption:     caption,
			ReplyMarkup: markup,
		})
	}
	return err
}

func sendDocumentBytesWithMarkup(ctx context.Context, api *telegrambot.Bot, chatID int64, filename string, data []byte, caption string, markup models.ReplyMarkup) error {
	if strings.TrimSpace(filename) == "" {
		filename = "document.txt"
	}
	_, err := api.SendDocument(ctx, &telegrambot.SendDocumentParams{
		ChatID: chatID,
		Document: &models.InputFileUpload{
			Filename: filename,
			Data:     bytes.NewReader(data),
		},
		Caption:     markdownToHTML(caption),
		ParseMode:   models.ParseModeHTML,
		ReplyMarkup: markup,
	})
	if err != nil {
		_, err = api.SendDocument(ctx, &telegrambot.SendDocumentParams{
			ChatID: chatID,
			Document: &models.InputFileUpload{
				Filename: filename,
				Data:     bytes.NewReader(data),
			},
			Caption:     caption,
			ReplyMarkup: markup,
		})
	}
	return err
}

func sendMessageWithReply(ctx context.Context, api *telegrambot.Bot, chatID int64, replyToMessageID int, text string) error {
	_, err := api.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID:    chatID,
		Text:      markdownToHTML(text),
		ParseMode: models.ParseModeHTML,
		ReplyParameters: &models.ReplyParameters{
			MessageID: replyToMessageID,
		},
	})
	if err != nil {
		_, err = api.SendMessage(ctx, &telegrambot.SendMessageParams{
			ChatID: chatID,
			Text:   text,
			ReplyParameters: &models.ReplyParameters{
				MessageID: replyToMessageID,
			},
		})
	}
	return err
}

func SendText(ctx context.Context, token string, chatID int64, text string) error {
	_, err := SendTextMessageID(ctx, token, chatID, text)
	return err
}

func SendTextMessageID(ctx context.Context, token string, chatID int64, text string) (int, error) {
	api, err := telegrambot.New(token, telegrambot.WithHTTPClient(20*time.Second, &http.Client{Timeout: 20 * time.Second}))
	if err != nil {
		return 0, err
	}
	message, err := api.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID:    chatID,
		Text:      markdownToHTML(text),
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		message, err = api.SendMessage(ctx, &telegrambot.SendMessageParams{
			ChatID: chatID,
			Text:   text,
		})
	}
	if err != nil {
		return 0, err
	}
	return message.ID, nil
}

func SendPhotoBytes(ctx context.Context, token string, chatID int64, filename string, data []byte, caption string) error {
	_, err := SendPhotoBytesMessageID(ctx, token, chatID, filename, data, caption)
	return err
}

func SendPhotoBytesMessageID(ctx context.Context, token string, chatID int64, filename string, data []byte, caption string) (int, error) {
	api, err := telegrambot.New(token, telegrambot.WithHTTPClient(20*time.Second, &http.Client{Timeout: 20 * time.Second}))
	if err != nil {
		return 0, err
	}
	message, err := api.SendPhoto(ctx, &telegrambot.SendPhotoParams{
		ChatID: chatID,
		Photo: &models.InputFileUpload{
			Filename: filename,
			Data:     bytes.NewReader(data),
		},
		Caption:   markdownToHTML(caption),
		ParseMode: models.ParseModeHTML,
	})
	if err != nil {
		message, err = api.SendPhoto(ctx, &telegrambot.SendPhotoParams{
			ChatID: chatID,
			Photo: &models.InputFileUpload{
				Filename: filename,
				Data:     bytes.NewReader(data),
			},
			Caption: caption,
		})
	}
	if err != nil {
		return 0, err
	}
	return message.ID, nil
}

func markdownToHTML(md string) string {
	escaped := html.EscapeString(md)

	// Block pre code: ```(lang)?\n(content)``` -> <pre>(content)</pre>
	rePre := regexp.MustCompile("(?s)```[a-zA-Z0-9]*\n?(.*?)```")
	escaped = rePre.ReplaceAllString(escaped, "<pre>${1}</pre>")

	// Inline code: `code` -> <code>code</code>
	reCode := regexp.MustCompile("`([^`\n]+)`")
	escaped = reCode.ReplaceAllString(escaped, "<code>${1}</code>")

	// Bold: **text** -> <b>text</b>
	reBold1 := regexp.MustCompile(`\*\*([^\*]+)\*\*`)
	escaped = reBold1.ReplaceAllString(escaped, "<b>${1}</b>")

	// Bold: __text__ -> <b>text</b>
	reBold2 := regexp.MustCompile(`__([^_]+)__`)
	escaped = reBold2.ReplaceAllString(escaped, "<b>${1}</b>")

	// Italic: *text* -> <i>text</i>
	reItalic1 := regexp.MustCompile(`\*([^\*]+)\*`)
	escaped = reItalic1.ReplaceAllString(escaped, "<i>${1}</i>")

	// Italic: _text_ -> <i>text</i>
	reItalic2 := regexp.MustCompile(`_([^_]+)_`)
	escaped = reItalic2.ReplaceAllString(escaped, "<i>${1}</i>")

	// Links: [text](url) -> <a href="url">text</a>
	reLink := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	escaped = reLink.ReplaceAllString(escaped, `<a href="${2}">${1}</a>`)

	// Headers: # Header -> <b>Header</b>
	lines := strings.Split(escaped, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			hashes := 0
			for _, r := range trimmed {
				if r == '#' {
					hashes++
				} else {
					break
				}
			}
			if hashes > 0 && hashes < len(trimmed) && trimmed[hashes] == ' ' {
				headerText := strings.TrimSpace(trimmed[hashes:])
				lines[i] = "<b>" + headerText + "</b>"
			}
		}
	}
	escaped = strings.Join(lines, "\n")

	return escaped
}
