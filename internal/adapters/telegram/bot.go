package telegram

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type Bot struct {
	token       string
	client      *http.Client
	sessions    map[int64]*preferenceSession
	store       ProfileStore
	parser      preferenceparser.Service
	geometry    preferenceparser.GeometryBuilder
	mapRenderer googlemaps.StaticMapRenderer
	mu          sync.Mutex
}

type ProfileStore interface {
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	SaveSearchProfile(ctx context.Context, profile domain.UserSearchProfile) error
	UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error)
	RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error)
	SaveListingFeedback(ctx context.Context, feedback domain.ListingFeedback) (string, bool, error)
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
	if update.Message.ReplyToMessage != nil && b.getSession(chatID) == nil && !strings.HasPrefix(strings.TrimSpace(update.Message.Text), "/") {
		reply, err := b.saveReplyFeedback(ctx, update.Message)
		if err != nil {
			_ = sendMessage(ctx, api, chatID, "Error: "+err.Error())
			return
		}
		_ = sendMessageWithReply(ctx, api, chatID, update.Message.ID, reply)
		return
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
	if len(response.PhotoBytes) > 0 {
		return sendPhotoBytesWithMarkup(ctx, api, chatID, response.PhotoFilename, response.PhotoBytes, response.Text, response.ReplyMarkup)
	}
	return sendMessageWithMarkup(ctx, api, chatID, response.Text, response.ReplyMarkup)
}

func (b *Bot) handleText(chatID int64, text string) (string, error) {
	response, err := b.handleTextResponse(chatID, text)
	return response.Text, err
}

type textResponse struct {
	Text          string
	ReplyMarkup   models.ReplyMarkup
	PhotoFilename string
	PhotoBytes    []byte
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
	if looksLikeChannelSubscription(text) {
		reply, err := b.subscribeChannelsFromText(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: mainCommandKeyboard()}, err
	}
	if text == "/start" || text == "/preferences" || text == "/restart" {
		b.setSession(chatID, &preferenceSession{State: "awaiting_query"})
		return textResponse{Text: preferencesStartMessage, ReplyMarkup: mainCommandKeyboard()}, nil
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
				return textResponse{Text: "Не смог разобрать GeoJSON: " + err.Error() + "\n\nОткрой карту Батуми, нарисуй Polygon/MultiPolygon, скопируй GeoJSON и пришли его сюда:\n" + batumiGeoJSONEditorURL, ReplyMarkup: confirmationKeyboard()}, nil
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
			return textResponse{Text: "Открой карту Батуми, нарисуй или поправь зону, скопируй GeoJSON из правой панели и пришли его сюда.\n\n" + batumiGeoJSONEditorURL + "\n\nПоддерживаются GeoJSON Polygon, MultiPolygon, Feature и FeatureCollection.", ReplyMarkup: confirmationKeyboard()}, nil
		case "cancel":
			b.deleteSession(chatID)
			return textResponse{Text: "Настройка отменена.", ReplyMarkup: mainCommandKeyboard()}, nil
		default:
			return textResponse{Text: "Выбери Save, Edit, Edit area или Cancel. Можно также прислать GeoJSON с Polygon/MultiPolygon.", ReplyMarkup: confirmationKeyboard()}, nil
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
		lines = append(lines, "https://t.me/batumi_rentals")
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
		return "Источники пока не подключены.\n\nВступи вручную в подходящие Telegram-чаты/группы, затем пришли сюда public ссылки или usernames, например:\nhttps://t.me/batumi_rentals\n@another_rent_channel\n\nАвтоматически вступать в чаты я не буду.", nil
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
	if len(parts) == 1 || strings.EqualFold(parts[1], "status") {
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
		return "Использование: /debug, /debug on или /debug off", nil
	}
}

func (b *Bot) saveReplyFeedback(ctx context.Context, message *models.Message) (string, error) {
	if b.store == nil {
		return "", fmt.Errorf("profile store is required")
	}
	if message.ReplyToMessage == nil {
		return "Ответь на сообщение с отчетом по квартире и напиши отзыв.", nil
	}
	text := strings.TrimSpace(message.Text)
	if text == "" {
		return "Пустой отзыв не сохранен.", nil
	}
	id, ok, err := b.store.SaveListingFeedback(ctx, domain.ListingFeedback{
		TelegramChatID:    message.Chat.ID,
		FeedbackMessageID: message.ID,
		RepliedMessageID:  message.ReplyToMessage.ID,
		Text:              text,
	})
	if err != nil {
		return "", err
	}
	if !ok {
		return "Не нашел объявление для этого ответа. Ответь на сообщение с отчетом по квартире, например: «гео определено неверно».", nil
	}
	return "Отзыв сохранен: feedback #" + id, nil
}

func debugModeStatusMessage(enabled bool) string {
	if enabled {
		return "Debug mode: on\n\nЯ пересылаю отчеты по обработанным объявлениям: прошедшим, отклоненным, дубликатам или ошибкам. Сообщения со статусом «не объявление о квартире» в чат не отправляю. Выключить: /debug off"
	}
	return "Debug mode: off\n\nЯ пересылаю только объявления, которые прошли фильтры. Включить все отчеты: /debug on"
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
	area, err := b.geometry.BuildSearchGeometry(ctx, draft.Preferences.LocationPreferences)
	if err != nil {
		return nil, err
	}
	if area != nil {
		draft.Preferences.UnresolvedLocationRules = append(draft.Preferences.UnresolvedLocationRules, area.UnresolvedRules...)
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

func (b *Bot) saveProfile(ctx context.Context, chatID int64, prefs domain.UserPreferences) error {
	return b.store.UpsertProfile(ctx, profiles.Profile{
		ID:             profiles.IDForTelegramChat(chatID),
		TelegramChatID: chatID,
		Preferences:    &prefs,
	})
}

func (b *Bot) saveDraft(ctx context.Context, chatID int64, draft preferenceparser.Draft, area *domain.GeoSearchArea) error {
	userID := profiles.IDForTelegramChat(chatID)
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
		Caption:     caption,
		ReplyMarkup: markup,
	})
	return err
}

func sendMessageWithReply(ctx context.Context, api *telegrambot.Bot, chatID int64, replyToMessageID int, text string) error {
	_, err := api.SendMessage(ctx, &telegrambot.SendMessageParams{
		ChatID: chatID,
		Text:   text,
		ReplyParameters: &models.ReplyParameters{
			MessageID: replyToMessageID,
		},
	})
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
		ChatID: chatID,
		Text:   text,
	})
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
		Caption: caption,
	})
	if err != nil {
		return 0, err
	}
	return message.ID, nil
}
