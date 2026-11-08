package telegram

import (
	"bytes"
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
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
	chatLocks        map[int64]*sync.Mutex
	accessStore      AccessStore
	admins           adminRegistry
	accessThrottle   accessThrottle
	notifier         notifier
	allowedChats     []int64
	allowedUsernames []string
	defaultCity      cities.City
	channelAccess    ChannelAccessChecker
	username         string
	widgetEndpoint   string
	getMeEndpoint    string
}

type ChannelAccessChecker interface {
	ChannelAccess(ctx context.Context, username string) (sources.ChannelAccess, error)
}

type ProfileStore interface {
	UpsertProfile(ctx context.Context, profile profiles.Profile) error
	GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error)
	SaveSearchProfile(ctx context.Context, profile domain.UserSearchProfile) error
	UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error)
	RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error)
	SaveListingFeedback(ctx context.Context, feedback domain.ListingFeedback) (string, *domain.ApartmentListing, error)
	ListingFeedbackExists(ctx context.Context, telegramChatID int64, feedbackMessageID int) (bool, error)
}

type preferenceSession struct {
	StartedAt  time.Time
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

func WithChannelAccessChecker(checker ChannelAccessChecker) Option {
	return func(bot *Bot) {
		bot.channelAccess = checker
	}
}

func New(token, outputDir string, opts ...Option) *Bot {
	bot := &Bot{
		token:       token,
		client:      &http.Client{Timeout: 20 * time.Second},
		sessions:    map[int64]*preferenceSession{},
		chatLocks:   map[int64]*sync.Mutex{},
		defaultCity: cities.Batumi,
	}
	for _, opt := range opts {
		opt(bot)
	}
	if bot.mcpAgent != nil {
		bot.mcpAgent.SetSourceManager(bot)
		bot.mcpAgent.SetAccessManager(bot)
		bot.mcpAgent.SetSettingsWidgetSender(bot)
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
		return redactTokenError(err, b.token)
	}
	b.notifier = func(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) error {
		return sendMessageWithMarkup(ctx, api, chatID, text, markup)
	}
	_, _ = api.SetMyCommands(ctx, &telegrambot.SetMyCommandsParams{Commands: botMenuCommands})
	api.Start(ctx)
	return nil
}

var botMenuCommands = []models.BotCommand{
	{Command: "start", Description: "Начать или продолжить настройку поиска"},
	{Command: "settings", Description: "Открыть настройки поиска"},
	{Command: "reset", Description: "Начать разговор заново, фильтры сохранятся"},
}

func (b *Bot) HandleText(chatID int64, text string) (string, error) {
	return b.handleText(chatID, text)
}

func (b *Bot) handleUpdate(ctx context.Context, api *telegrambot.Bot, update *models.Update) {
	if update.CallbackQuery != nil {
		b.handleCallback(ctx, api, update.CallbackQuery)
		return
	}
	if update.Message == nil || update.Message.Text == "" {
		return
	}

	if !isPrivateChatMessage(update.Message) {
		return
	}
	chatID := update.Message.Chat.ID
	chatLock := b.chatLock(chatID)
	chatLock.Lock()
	defer chatLock.Unlock()
	if !b.authorizeMessage(ctx, update.Message) {
		return
	}
	sendTyping(ctx, api, chatID)
	if update.Message.ReplyToMessage != nil && b.getSession(chatID) == nil && !strings.HasPrefix(strings.TrimSpace(update.Message.Text), "/") {
		reply, handled, err := b.saveReplyFeedback(ctx, update.Message)
		if err != nil {
			_ = sendMessage(ctx, api, chatID, friendlyFailure("reply feedback", chatID, err))
			return
		}
		if handled {
			_ = sendMessageWithReply(ctx, api, chatID, update.Message.ID, reply)
			return
		}
	}
	if err := b.handleMessage(ctx, api, chatID, update.Message.Text); err != nil {
		_ = sendMessage(ctx, api, chatID, friendlyFailure("message", chatID, err))
	}
}

func (b *Bot) handleMessage(ctx context.Context, api *telegrambot.Bot, chatID int64, text string) error {
	response, err := b.handleTextResponse(chatID, text)
	if err != nil {
		return err
	}
	if response.Widget != nil {
		err := b.postWidget(ctx, chatID, *response.Widget)
		if err == nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "telegram bot settings widget failed for chat %d: %v\n", chatID, err)
		return sendMessageWithMarkup(ctx, api, chatID, response.Text, response.ReplyMarkup)
	}
	if len(response.DocumentBytes) > 0 {
		return sendDocumentBytesWithMarkup(ctx, api, chatID, response.DocumentFilename, response.DocumentBytes, response.Text, response.ReplyMarkup)
	}
	if len(response.PhotoBytes) > 0 {
		if telegramCaptionFits(response.Text) {
			return sendPhotoBytesWithMarkup(ctx, api, chatID, response.PhotoFilename, response.PhotoBytes, response.Text, response.ReplyMarkup)
		}
		if err := sendPhotoBytesWithMarkup(ctx, api, chatID, response.PhotoFilename, response.PhotoBytes, searchAreaMapHint, nil); err != nil {
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
	Widget           *settingsWidget
}

func (b *Bot) handleTextResponse(chatID int64, text string) (textResponse, error) {
	if b.store == nil {
		return textResponse{}, fmt.Errorf("profile store is required")
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/channels") {
		reply, err := b.listChannels(context.Background(), chatID)
		return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
	}
	if strings.HasPrefix(text, "/subscribe") {
		reply, err := b.subscribeChannel(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
	}
	if strings.HasPrefix(text, "/unsubscribe") {
		reply, err := b.unsubscribeChannel(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
	}
	if strings.HasPrefix(text, "/debug") {
		reply, err := b.debugMode(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
	}
	if text == "/city" || strings.HasPrefix(text, "/city ") {
		reply, err := b.cityCommand(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
	}
	if strings.HasPrefix(text, "/mcp") {
		b.deleteSession(chatID)
		return textResponse{Text: conversationModeNote, ReplyMarkup: removeKeyboard()}, nil
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
			ReplyMarkup: removeKeyboard(),
		}, nil
	}
	if b.mcpAgent == nil && looksLikeChannelSubscription(text) {
		reply, err := b.subscribeChannelsFromText(context.Background(), chatID, text)
		return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
	}
	if isWidgetSettingsStart(text) {
		return b.applyWidgetSettingsStart(context.Background(), chatID, text)
	}
	if text == "/settings" || strings.HasPrefix(text, "/settings@") {
		return b.settingsWidgetResponse(context.Background(), chatID)
	}
	if text == "/start" || text == "/preferences" || text == "/restart" {
		if b.mcpAgent != nil {
			return b.startConversation(context.Background(), chatID)
		}
		b.setSession(chatID, &preferenceSession{State: "awaiting_query"})
		return textResponse{Text: preferencesStartMessage, ReplyMarkup: removeKeyboard()}, nil
	}

	session := b.getSession(chatID)
	if session == nil && b.mcpAgent != nil {
		profile, err := b.ensureProfile(context.Background(), chatID)
		if err != nil {
			return textResponse{}, err
		}
		reply, prefsChanged, areaChanged, err := b.mcpAgent.ProcessCommand(context.Background(), profile.ID, text)
		if err != nil {
			return textResponse{Text: friendlyFailure("agent", chatID, err), ReplyMarkup: removeKeyboard()}, nil
		}
		if prefsChanged != nil || areaChanged != nil {
			var draft preferenceparser.Draft
			if prefsChanged != nil {
				normalized := preferenceparser.NormalizePreferences(*prefsChanged)
				draft.Preferences = normalized
			} else if profile.Preferences != nil {
				draft.Preferences = *profile.Preferences
			}
			cityNote, area := b.applyAgentCityChange(profile, &draft, areaChanged)
			area, err = b.agentSearchArea(context.Background(), profile, &draft, area)
			if err != nil {
				return textResponse{Text: friendlyFailure("search area", chatID, err), ReplyMarkup: removeKeyboard()}, nil
			}
			if err := b.saveDraft(context.Background(), chatID, draft, area); err != nil {
				return textResponse{Text: friendlyFailure("save filters", chatID, err), ReplyMarkup: removeKeyboard()}, nil
			}
			reply += cityNote
			if profile.Preferences == nil && prefsChanged != nil && b.hasSources(context.Background(), chatID) {
				reply = strings.TrimSpace(reply) + "\n\n" + searchStartedNote
			}
		}
		resp := textResponse{
			Text:        reply,
			ReplyMarkup: removeKeyboard(),
		}
		if shouldAttachMCPMap(text, reply, prefsChanged != nil || areaChanged != nil) {
			if saStore, ok := b.store.(interface {
				LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error)
			}); ok {
				if geoJSON, ok, err := saStore.LatestSearchAreaGeoJSON(context.Background(), profile.ID); err == nil && ok {
					if mapImage, err := b.mapRenderer.SearchAreaImage(context.Background(), geoJSON); err == nil && len(mapImage) > 0 {
						resp.PhotoFilename = "search-area-map.png"
						resp.PhotoBytes = mapImage
						resp.Text = withSearchAreaMapHint(resp.Text)
					}
				}
			}
		}
		return resp, nil
	}

	if session == nil {
		return textResponse{Text: "Отправь /start, чтобы настроить поиск квартиры.", ReplyMarkup: removeKeyboard()}, nil
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
			session.Draft.Preferences.HasSearchArea = true
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
			return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, err
		case "edit":
			session.State = "awaiting_query"
			b.setSession(chatID, session)
			return textResponse{Text: "Отправь исправленный запрос одним сообщением.", ReplyMarkup: removeKeyboard()}, nil
		case "edit_area":
			return searchAreaEditResponse(session.SearchArea, b.cityOf(session.Draft.Preferences)), nil
		case "cancel":
			b.deleteSession(chatID)
			return textResponse{Text: "Настройка отменена.", ReplyMarkup: removeKeyboard()}, nil
		default:
			return textResponse{Text: "Выбери Save, Edit, Edit area или Cancel. Можно также отправить GeoJSON прямо сюда одним сообщением.", ReplyMarkup: confirmationKeyboard()}, nil
		}
	default:
		b.deleteSession(chatID)
		return textResponse{Text: "Отправь /start, чтобы настроить поиск квартиры.", ReplyMarkup: removeKeyboard()}, nil
	}
}

const (
	newUserGreeting = `Привет! Я ищу квартиры в аренду: читаю Telegram-каналы с объявлениями и присылаю только те, что тебе подходят.

Настроим поиск прямо в переписке — отвечай обычными сообщениями. Можно сразу всё одним текстом, например: «Батуми, до 600$, от 45 м², рядом с Новым бульваром, с кондиционером, без советского ремонта».

Или по шагам. Для начала: в каком городе ищем, Батуми или Тбилиси, и какой бюджет в месяц?`
	returningUserGreeting = `Привет! Поиск уже настроен. Просто напиши, что поменять: например, «подними бюджет до 700$», «добавь канал @name» или «присылай только подходящие». Чтобы увидеть текущие настройки и карту зоны, спроси «покажи мой поиск».`
	searchStartedNote     = `✅ Дальше я работаю сам: проверяю новые объявления в подключенных источниках и присылаю сюда только подходящие. Поменять поиск можно в любой момент — просто напиши.`
	conversationModeNote  = `Отдельный режим больше не нужен: просто пиши мне обычными сообщениями, я пойму и сам поменяю настройки. /reset начинает разговор заново, фильтры при этом сохраняются.`
)

const searchAreaMapHint = "🗺 На карте — зона, где я ищу. Если нужно шире, уже или в другом месте, просто напиши."

func withSearchAreaMapHint(text string) string {
	withHint := strings.TrimSpace(text) + "\n\n" + searchAreaMapHint
	if !telegramCaptionFits(withHint) {
		return text
	}
	return withHint
}

func (b *Bot) chatLock(chatID int64) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	lock, ok := b.chatLocks[chatID]
	if !ok {
		lock = &sync.Mutex{}
		b.chatLocks[chatID] = lock
	}
	return lock
}

func (b *Bot) ensureProfile(ctx context.Context, chatID int64) (profiles.Profile, error) {
	profileID := profiles.IDForTelegramChat(chatID)
	profile, ok, err := b.store.GetProfile(ctx, profileID)
	if err != nil || (ok && profile.TelegramChatID == chatID) {
		return profile, err
	}
	update := profiles.Profile{ID: profileID, TelegramChatID: chatID}
	if !ok {
		matchesOnly := false
		update.DebugMode = &matchesOnly
	}
	if err := b.store.UpsertProfile(ctx, update); err != nil {
		return profiles.Profile{}, err
	}
	profile, _, err = b.store.GetProfile(ctx, profileID)
	return profile, err
}

func (b *Bot) startConversation(ctx context.Context, chatID int64) (textResponse, error) {
	b.deleteSession(chatID)
	profile, err := b.ensureProfile(ctx, chatID)
	if err != nil {
		return textResponse{}, err
	}
	greeting := returningUserGreeting
	if profile.Preferences == nil {
		greeting = newUserGreeting
	}
	now := time.Now()
	history := append(slices.Clone(profile.MCPHistory),
		profiles.MCPMessage{Role: "user", Content: "/start", CreatedAt: now},
		profiles.MCPMessage{Role: "assistant", Content: greeting, CreatedAt: now},
	)
	if err := b.store.UpsertProfile(ctx, profiles.Profile{ID: profile.ID, TelegramChatID: chatID, MCPHistory: history}); err != nil {
		return textResponse{}, err
	}
	return textResponse{Text: greeting, ReplyMarkup: removeKeyboard()}, nil
}

func (b *Bot) hasSources(ctx context.Context, chatID int64) bool {
	sources, err := b.channelList(ctx, chatID)
	return err == nil && len(sources) > 0
}

type popularSourceStore interface {
	PopularSources(ctx context.Context, profileID, city string, limit int) ([]profiles.PopularSource, error)
}

const popularSourcesLimit = 5

func (b *Bot) SuggestSources(ctx context.Context, chatID int64, cityKey string) (string, error) {
	store, ok := b.store.(popularSourceStore)
	if !ok {
		return "no suggestions available", nil
	}
	city, ok := cities.Find(cityKey)
	if !ok {
		city = b.profileCity(ctx, chatID)
	}
	popular, err := store.PopularSources(ctx, profiles.IDForTelegramChat(chatID), city.Key, popularSourcesLimit)
	if err != nil {
		return "", err
	}
	if len(popular) == 0 {
		return "no popular sources for " + city.Key + " yet", nil
	}
	lines := []string{"Popular sources in " + city.Key + " that other users already read (not connected for this user yet):"}
	for _, source := range popular {
		line := "- @" + source.Username
		if source.Title != "" {
			line += " — " + source.Title
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) ListSources(ctx context.Context, chatID int64) ([]string, error) {
	return b.channelList(ctx, chatID)
}

func (b *Bot) AddSources(ctx context.Context, chatID int64, refs []string) (string, error) {
	var results []string
	for _, ref := range refs {
		username, ok := parsePublicTelegramUsername(ref)
		if !ok || username == "" {
			results = append(results, ref+": не подключен. "+unsupportedChannelRefMessage(ref))
			continue
		}
		reply, err := b.upsertChannel(ctx, chatID, username, "")
		if err != nil {
			return "", err
		}
		results = append(results, reply)
	}
	if len(results) == 0 {
		return "no sources given", nil
	}
	return strings.Join(results, "\n"), nil
}

func (b *Bot) RemoveSources(ctx context.Context, chatID int64, refs []string) (string, error) {
	var results []string
	for _, ref := range refs {
		username, ok := parsePublicTelegramUsername(ref)
		if !ok || username == "" {
			results = append(results, ref+": не похоже на @username или ссылку t.me/name")
			continue
		}
		removed, err := b.store.RemoveProfileChannel(ctx, profiles.IDForTelegramChat(chatID), username)
		if err != nil {
			return "", err
		}
		if removed {
			results = append(results, "Источник @"+username+" отключен")
		} else {
			results = append(results, "Источник @"+username+" не был подключен")
		}
	}
	if len(results) == 0 {
		return "no sources given", nil
	}
	return strings.Join(results, "\n"), nil
}

func (b *Bot) afterPreferencesSavedMessage(ctx context.Context, chatID int64) (string, error) {
	channels, err := b.channelList(ctx, chatID)
	if err != nil {
		return "", err
	}
	var lines []string
	lines = append(lines, "Готово, фильтры сохранены.")
	lines = append(lines, "")
	lines = append(lines, "Теперь нужны источники объявлений: публичные Telegram-каналы и группы с арендой. Пришли сюда ссылки t.me/name или @username.")
	lines = append(lines, "")
	lines = append(lines, readerAccountNote)
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
		return "Источники пока не подключены.\n\nПришли публичные ссылки или usernames Telegram-каналов и групп с объявлениями, например:\nhttps://t.me/rent_channel\n@another_rent_channel\n\n" + readerAccountNote, nil
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
	lines = append(lines, "", readerAccountNote, "Список источников: /channels")
	return strings.Join(lines, "\n"), nil
}

func (b *Bot) upsertChannel(ctx context.Context, chatID int64, username, title string) (string, error) {
	username = strings.TrimPrefix(strings.TrimSpace(username), "@")
	accessNote := ""
	if b.channelAccess != nil {
		checkCtx, cancel := context.WithTimeout(ctx, channelAccessTimeout)
		access, err := b.channelAccess.ChannelAccess(checkCtx, username)
		cancel()
		switch {
		case err != nil:
			accessNote = ". Не смог проверить доступ к этому чату, проверю позже при чтении."
		case !access.Found:
			return "Не нашел публичный канал или группу @" + username + ". Проверь username.", nil
		case !access.Member:
			accessNote = ". Аккаунт-читатель владельца бота не состоит в этом чате, поэтому новые объявления оттуда не придут, пока владелец бота в него не вступит."
		}
		if err == nil && title == "" {
			title = access.Title
		}
	}
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
		return "Добавлен @" + username + accessNote, nil
	}
	return "Обновлен @" + username + accessNote, nil
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

func (b *Bot) saveReplyFeedback(ctx context.Context, message *models.Message) (string, bool, error) {
	if message.ReplyToMessage == nil {
		return "Ответь на сообщение с отчетом по квартире и напиши отзыв.", false, nil
	}
	return b.saveListingFeedback(ctx, message.Chat.ID, message.ID, message.ReplyToMessage.ID, message.Text)
}

func (b *Bot) saveListingFeedback(ctx context.Context, chatID int64, feedbackMessageID int, notificationMessageID int, text string) (string, bool, error) {
	if b.store == nil {
		return "", false, fmt.Errorf("profile store is required")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "Пустой отзыв не сохранен.", false, nil
	}
	id, listing, err := b.store.SaveListingFeedback(ctx, domain.ListingFeedback{
		TelegramChatID:    chatID,
		FeedbackMessageID: feedbackMessageID,
		RepliedMessageID:  notificationMessageID,
		Text:              text,
	})
	if err != nil {
		return "", false, err
	}
	if listing == nil {
		return "", false, nil
	}

	profile, profileOk, profileErr := b.store.GetProfile(ctx, profiles.IDForTelegramChat(chatID))
	if profileErr == nil && profileOk && b.mcpAgent != nil {
		reply, prefsChanged, areaChanged, agentErr := b.mcpAgent.ProcessFeedback(ctx, profile.ID, *listing, text, chatID, notificationMessageID)
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
			draft.Preferences.City = b.storedCity(profile).Key
			area, areaErr := b.agentSearchArea(ctx, profile, &draft, areaChanged)
			if areaErr != nil {
				return "Отзыв сохранен, но ошибка построения зоны поиска: " + areaErr.Error(), true, nil
			}
			if draftErr := b.saveDraft(ctx, chatID, draft, area); draftErr != nil {
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

const (
	channelAccessTimeout = 10 * time.Second
	preferenceSessionTTL = 30 * time.Minute
)

const readerAccountNote = "Объявления читает аккаунт-читатель владельца бота, а я не вступаю в чаты автоматически. Новые объявления приходят только из чатов, где этот аккаунт состоит; если его там нет, я предупрежу при подключении."

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
	return "Пока я могу подключать только public Telegram-источники по @username или t.me/name.\n\nПриватные чаты по invite-ссылкам вида t.me/+... не поддерживаются: нужен публичный @username канала или группы. Автоматическое вступление не выполняю."
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
		draft.Preferences.UnresolvedLocationRules = appendMissing(draft.Preferences.UnresolvedLocationRules, area.UnresolvedRules...)
		draft.Preferences.HasSearchArea = strings.TrimSpace(area.GeoJSON) != ""
	}
	return area, nil
}

func appendMissing(values []string, additions ...string) []string {
	for _, addition := range additions {
		if !slices.Contains(values, addition) {
			values = append(values, addition)
		}
	}
	return values
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

func (b *Bot) agentSearchArea(ctx context.Context, before profiles.Profile, draft *preferenceparser.Draft, area *domain.GeoSearchArea) (*domain.GeoSearchArea, error) {
	if area != nil || b.geometry == nil || b.keepsSearchLocations(before.Preferences, draft.Preferences) {
		return area, nil
	}
	return b.prepareDraftGeometry(ctx, draft)
}

func (b *Bot) keepsSearchLocations(stored *domain.UserPreferences, next domain.UserPreferences) bool {
	if stored == nil || b.cityOf(*stored).Key != b.cityOf(next).Key {
		return false
	}
	storedKeys := locationGeometryKeys(preferenceparser.NormalizePreferences(*stored).LocationPreferences)
	nextKeys := locationGeometryKeys(next.LocationPreferences)
	if len(storedKeys) == 0 && len(nextKeys) == 0 {
		return true
	}
	return stored.HasSearchArea && slices.Equal(storedKeys, nextKeys)
}

func locationGeometryKeys(locations []domain.LocationPreference) []string {
	keys := make([]string, 0, len(locations))
	for _, location := range locations {
		radius := location.MaxDistanceMeters
		if location.RadiusMeters != nil {
			radius = *location.RadiusMeters
		}
		coordinates := ""
		if location.Latitude != 0 || location.Longitude != 0 {
			coordinates = fmt.Sprintf("%.5f,%.5f", location.Latitude, location.Longitude)
		}
		keys = append(keys, strings.Join([]string{
			normalizedText(location.Name),
			normalizedText(location.CenterName),
			normalizedText(location.Type),
			strconv.Itoa(radius),
			normalizedText(stringOrEmpty(location.BoundaryStreetName)),
			normalizedText(stringOrEmpty(location.BoundaryRule)),
			coordinates,
		}, "|"))
	}
	return keys
}

func normalizedText(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func (b *Bot) cityOf(prefs domain.UserPreferences) cities.City {
	return cities.ForKey(prefs.City, b.defaultCity)
}

func (b *Bot) storedCity(profile profiles.Profile) cities.City {
	if profile.Preferences == nil {
		return b.defaultCity
	}
	return b.cityOf(*profile.Preferences)
}

func (b *Bot) applyAgentCityChange(profile profiles.Profile, draft *preferenceparser.Draft, area *domain.GeoSearchArea) (string, *domain.GeoSearchArea) {
	previous := b.storedCity(profile)
	if _, ok := cities.Find(draft.Preferences.City); !ok {
		draft.Preferences.City = previous.Key
		return "", area
	}
	next := b.cityOf(draft.Preferences)
	if next.Key == previous.Key {
		return "", area
	}
	var previousLocations []domain.LocationPreference
	if profile.Preferences != nil {
		previousLocations = profile.Preferences.LocationPreferences
	}
	draft.Preferences.UnresolvedLocationRules = nil
	draft.Preferences.HasSearchArea = false
	draft.Preferences.LocationPreferences = locationsNotIn(draft.Preferences.LocationPreferences, previousLocations)
	if len(draft.Preferences.LocationPreferences) > 0 {
		draft.Preferences.Locations = nil
		return fmt.Sprintf("\n\nГород поиска: %s.", next.DisplayName), nil
	}
	draft.Preferences.LocationPreferences = nil
	draft.Preferences.Locations = nil
	return fmt.Sprintf("\n\nГород поиска: %s. Районы и зона поиска от прошлого города сброшены, сейчас подходят объявления из любого района.", next.DisplayName), &domain.GeoSearchArea{SRID: 4326}
}

func locationsNotIn(locations, previous []domain.LocationPreference) []domain.LocationPreference {
	previousKeys := map[string]bool{}
	for _, location := range previous {
		for _, key := range locationKeys(location) {
			previousKeys[key] = true
		}
	}
	var fresh []domain.LocationPreference
	for _, location := range locations {
		known := false
		for _, key := range locationKeys(location) {
			if previousKeys[key] {
				known = true
				break
			}
		}
		if !known {
			fresh = append(fresh, location)
		}
	}
	return fresh
}

func locationKeys(location domain.LocationPreference) []string {
	var keys []string
	for _, value := range []string{location.Name, location.CenterName, location.RawText} {
		if key := strings.ToLower(strings.TrimSpace(value)); key != "" {
			keys = append(keys, key)
		}
	}
	return keys
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
		return fmt.Sprintf("Город поиска: %s. Фильтров пока нет: просто напиши, какую квартиру ищем.", city.DisplayName), nil
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
	return fmt.Sprintf("Город поиска: %s. Районы и зона поиска от прошлого города сброшены, сейчас подходят объявления из любого района. Чтобы задать районы, просто напиши, где хочешь жить.", city.DisplayName), nil
}

func (b *Bot) getSession(chatID int64) *preferenceSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	session := b.sessions[chatID]
	if session != nil && time.Since(session.StartedAt) > preferenceSessionTTL {
		delete(b.sessions, chatID)
		return nil
	}
	return session
}

func (b *Bot) setSession(chatID int64, session *preferenceSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	session.StartedAt = time.Now()
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

const failureReplyText = "Не получилось ответить: что-то сломалось на моей стороне. Попробуй еще раз через минуту, а если не поможет — напиши иначе."

func friendlyFailure(stage string, chatID int64, err error) string {
	fmt.Fprintf(os.Stderr, "telegram bot %s failed for chat %d: %s\n", stage, chatID, redactAnyBotToken(fmt.Sprint(err)))
	return failureReplyText
}

func sendTyping(ctx context.Context, api *telegrambot.Bot, chatID int64) {
	if api == nil {
		return
	}
	_, _ = api.SendChatAction(ctx, &telegrambot.SendChatActionParams{ChatID: chatID, Action: models.ChatActionTyping})
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
		return 0, redactTokenError(err, token)
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
		return 0, redactTokenError(err, token)
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
		return 0, redactTokenError(err, token)
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
		return 0, redactTokenError(err, token)
	}
	return message.ID, nil
}

var (
	markdownPreBlock    = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\n?(.*?)```")
	markdownInlineCode  = regexp.MustCompile("`([^`\n]+)`")
	markdownStarBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	markdownStarItalic  = regexp.MustCompile(`(^|[^\p{L}\p{N}_*])\*([^\s*][^*\n]*?)\*($|[^\p{L}\p{N}_*])`)
	markdownUnderBold   = regexp.MustCompile(`(^|[^\p{L}\p{N}_])__([^_\n]+)__($|[^\p{L}\p{N}_])`)
	markdownUnderItalic = regexp.MustCompile(`(^|[^\p{L}\p{N}_])_([^\s_][^_\n]*?)_($|[^\p{L}\p{N}_])`)
	markdownLink        = regexp.MustCompile(`\[([^\]\n]+)\]\(([^)\s]+)\)`)
	markdownURL         = regexp.MustCompile(`https?://[^\s<>"]+`)
	markdownMention     = regexp.MustCompile(`@\w+`)
	markdownPlaceholder = regexp.MustCompile("\x00(\\d+)\x00")
)

var markdownFormattingTag = regexp.MustCompile(`</?[bi]>`)

func wellNestedTags(text string) bool {
	var open []string
	for _, tag := range markdownFormattingTag.FindAllString(text, -1) {
		if !strings.HasPrefix(tag, "</") {
			open = append(open, tag[1:2])
			continue
		}
		if len(open) == 0 || open[len(open)-1] != tag[2:3] {
			return false
		}
		open = open[:len(open)-1]
	}
	return len(open) == 0
}

func replaceUntilStable(pattern *regexp.Regexp, text, replacement string) string {
	for {
		replaced := pattern.ReplaceAllString(text, replacement)
		if replaced == text {
			return text
		}
		text = replaced
	}
}

func markdownToHTML(md string) string {
	var protected []string
	protect := func(fragment string) string {
		protected = append(protected, fragment)
		return fmt.Sprintf("\x00%d\x00", len(protected)-1)
	}

	escaped := html.EscapeString(strings.ReplaceAll(md, "\x00", ""))
	escaped = markdownPreBlock.ReplaceAllStringFunc(escaped, func(block string) string {
		return protect("<pre>" + markdownPreBlock.FindStringSubmatch(block)[1] + "</pre>")
	})
	escaped = markdownInlineCode.ReplaceAllStringFunc(escaped, func(code string) string {
		return protect("<code>" + markdownInlineCode.FindStringSubmatch(code)[1] + "</code>")
	})
	escaped = markdownLink.ReplaceAllStringFunc(escaped, func(link string) string {
		parts := markdownLink.FindStringSubmatch(link)
		return protect(`<a href="` + parts[2] + `">` + parts[1] + `</a>`)
	})
	escaped = markdownURL.ReplaceAllStringFunc(escaped, protect)
	escaped = markdownMention.ReplaceAllStringFunc(escaped, protect)

	plain := escaped
	escaped = markdownStarBold.ReplaceAllString(escaped, "<b>${1}</b>")
	escaped = replaceUntilStable(markdownUnderBold, escaped, "${1}<b>${2}</b>${3}")
	escaped = replaceUntilStable(markdownStarItalic, escaped, "${1}<i>${2}</i>${3}")
	escaped = replaceUntilStable(markdownUnderItalic, escaped, "${1}<i>${2}</i>${3}")

	lines := strings.Split(escaped, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
		if hashes > 0 && hashes < len(trimmed) && trimmed[hashes] == ' ' {
			lines[i] = "<b>" + strings.TrimSpace(trimmed[hashes:]) + "</b>"
		}
	}
	escaped = strings.Join(lines, "\n")

	if !wellNestedTags(escaped) {
		escaped = plain
	}
	for markdownPlaceholder.MatchString(escaped) {
		escaped = markdownPlaceholder.ReplaceAllStringFunc(escaped, func(placeholder string) string {
			index, _ := strconv.Atoi(markdownPlaceholder.FindStringSubmatch(placeholder)[1])
			return protected[index]
		})
	}
	return escaped
}
