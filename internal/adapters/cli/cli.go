package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"apartment-hunter/internal/adapters/geocodingmock"
	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/adapters/mcpagent"
	"apartment-hunter/internal/adapters/openaipreferences"
	"apartment-hunter/internal/adapters/openaivision"
	"apartment-hunter/internal/adapters/telegram"
	"apartment-hunter/internal/adapters/telegramuserbot"
	"apartment-hunter/internal/adapters/visionmock"
	"apartment-hunter/internal/cities"
	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/evaluation"
	"apartment-hunter/internal/geocoding"
	"apartment-hunter/internal/interview"
	"apartment-hunter/internal/listingparser"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/preferences"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/scoring"
	"apartment-hunter/internal/sources"
	pgstore "apartment-hunter/internal/storage/postgres"
)

const telegramPhotoCaptionMaxUnits = 1024

type App struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

func New() App {
	return App{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}

func (a App) Run(args []string) error {
	if len(args) == 0 {
		return a.printHelp()
	}

	switch args[0] {
	case "interview":
		path := "preferences.yaml"
		if len(args) > 1 {
			path = args[1]
		}
		prefs, err := interview.NewWithResolver(a.In, a.Out, newLocationResolver()).Run()
		if err != nil {
			return err
		}
		if err := preferences.Save(path, prefs); err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Saved %s\n", path)
		return nil
	case "score":
		if len(args) != 3 {
			return fmt.Errorf("usage: apartment-hunter score sample_listing.json preferences.yaml")
		}
		return a.score(args[1], args[2])
	case "telegram":
		return a.telegram(args)
	case "telegram-read":
		profileID, err := profileIDArg(args, "telegram-read")
		if err != nil {
			return err
		}
		return a.telegramRead(profileID)
	case "telegram-process":
		profileID := ""
		if len(args) > 1 {
			profileID = strings.TrimSpace(args[len(args)-1])
		}
		return a.telegramProcess(profileID)
	case "help", "-h", "--help":
		return a.printHelp()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a App) telegramProcess(onlyProfileID string) error {
	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return a.telegramProcessWithContext(ctx, onlyProfileID, reader)
}

func (a App) telegram(args []string) error {
	outputDir := "preferences"
	if len(args) > 1 {
		outputDir = args[1]
	}
	token := os.Getenv("TELEGRAM_BOT_TOKEN")
	if token == "" {
		return fmt.Errorf("TELEGRAM_BOT_TOKEN is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openPostgresStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()

	if reason := telegramWorkerDisabledReason(); reason != "" {
		fmt.Fprintf(a.Out, "Telegram worker not started: %s\n", reason)
	} else {
		go func() {
			if err := a.runTelegramWorker(ctx, ""); err != nil {
				fmt.Fprintf(a.Err, "Telegram worker failed: %v\n", err)
			}
		}()
	}

	nlpParser, err := openaipreferences.FromEnv()
	if err != nil {
		return err
	}
	prefsParser := preferenceparser.Service{
		Normalizer: nlpParser,
		Extractor:  nlpParser,
	}
	geometry := preferenceparser.PostGISGeometryBuilder{Resolver: newGoogleGeocoder()}
	mapRenderer := googlemaps.StaticMapRenderer{APIKey: strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GOOGLE_MAPS_API_KEY"))}
	var userbot *telegramuserbot.Client
	if reason := telegramWorkerDisabledReason(); reason == "" {
		if userbot, err = telegramuserbot.FromEnv(a.In); err != nil {
			fmt.Fprintf(a.Err, "Failed to initialize Telegram userbot client: %v\n", err)
			userbot = nil
		}
	}
	mcpAgent, err := mcpagent.FromEnv(store)
	if err != nil {
		fmt.Fprintf(a.Err, "Failed to initialize MCP agent: %v\n", err)
	} else if mcpAgent != nil {
		mcpAgent.SetGeocoder(newGoogleGeocoder())
		if userbot != nil {
			mcpAgent.SetTelegramChatLister(userbot)
		}
	}
	botOptions := []telegram.Option{
		telegram.WithPreferencesParser(prefsParser),
		telegram.WithGeometryBuilder(geometry),
		telegram.WithSearchAreaMapRenderer(mapRenderer),
		telegram.WithProfileStore(store),
		telegram.WithMCPAgent(mcpAgent),
	}
	if userbot != nil {
		botOptions = append(botOptions, telegram.WithChannelAccessChecker(userbot))
	}
	fmt.Fprintf(a.Out, "Telegram bot started. Saving profiles to Postgres\n")

	allowedChatIDs := parseChatIDs(os.Getenv("TELEGRAM_ALLOWED_CHAT_IDS"))
	if profileID := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_PROFILE_ID")); profileID != "" {
		if strings.HasPrefix(profileID, "telegram_") {
			if id, err := strconv.ParseInt(strings.TrimPrefix(profileID, "telegram_"), 10, 64); err == nil {
				exists := false
				for _, existing := range allowedChatIDs {
					if existing == id {
						exists = true
						break
					}
				}
				if !exists {
					allowedChatIDs = append(allowedChatIDs, id)
				}
			}
		}
	}

	var allowedUsernames []string
	if allowedUsernamesEnv := strings.TrimSpace(os.Getenv("TELEGRAM_ALLOWED_USERNAMES")); allowedUsernamesEnv != "" {
		for _, part := range strings.Split(allowedUsernamesEnv, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				allowedUsernames = append(allowedUsernames, part)
			}
		}
	}

	botOptions = append(botOptions,
		telegram.WithAllowedChats(allowedChatIDs),
		telegram.WithAllowedUsernames(allowedUsernames),
		telegram.WithAccessStore(store),
		telegram.WithAdminChats(parseChatIDs(os.Getenv("TELEGRAM_ADMIN_CHAT_IDS"))),
	)
	err = telegram.New(token, outputDir, botOptions...).Run(ctx.Done())
	stop()
	return err
}

func (a App) runTelegramWorker(ctx context.Context, onlyProfileID string) error {
	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}

	for {
		err := a.telegramProcessWithContext(ctx, onlyProfileID, reader)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		fmt.Fprintf(a.Err, "Telegram worker stopped: %v; retrying in 15s\n", err)
		timer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func telegramWorkerDisabledReason() string {
	if strings.TrimSpace(os.Getenv("TELEGRAM_API_ID")) == "" {
		return "TELEGRAM_API_ID is not set"
	}
	if strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH")) == "" {
		return "TELEGRAM_API_HASH is not set"
	}
	if strings.TrimSpace(os.Getenv("TELEGRAM_PHONE")) == "" && strings.TrimSpace(os.Getenv("TELEGRAM_SESSION_STRING")) == "" {
		return "TELEGRAM_PHONE or TELEGRAM_SESSION_STRING is not set"
	}
	return ""
}

func (a App) telegramProcessWithContext(parent context.Context, onlyProfileID string, reader *telegramuserbot.Client) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	store, err := openPostgresStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	if onlyProfileID != "" {
		if err := requireProcessableProfile(ctx, store, onlyProfileID); err != nil {
			return err
		}
	}

	analyzer, err := openaivision.FromEnv()
	if err != nil {
		return err
	}
	pipeline := listingPipeline{
		store:                store,
		reader:               reader,
		analyzer:             analyzer,
		geocoder:             newGoogleGeocoder(),
		defaultCity:          cities.Batumi,
		geocodeLog:           newGeocodeLogger(a.Out),
		pipelineLog:          newPipelineLogger(a.Out),
		botToken:             strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")),
		mapRenderer:          googlemaps.StaticMapRenderer{APIKey: strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GOOGLE_MAPS_API_KEY"))},
		richNotifications:    richNotificationsEnabled(os.Getenv("TELEGRAM_RICH_NOTIFICATIONS")),
		collageNotifications: richNotificationsEnabled(os.Getenv("TELEGRAM_COLLAGE_NOTIFICATIONS")),
		out:                  a.Out,
		errOut:               a.Err,
	}
	worker := &listingWorker{
		store:           store,
		history:         reader,
		albumText:       reader,
		process:         pipeline.process,
		onlyProfileID:   onlyProfileID,
		out:             a.Out,
		errOut:          a.Err,
		caughtUp:        map[string]bool{},
		catchUpFailures: map[string]catchUpFailure{},
	}
	if onlyProfileID != "" {
		fmt.Fprintf(a.Out, "Listening for new Telegram messages for %s\n", onlyProfileID)
	} else {
		fmt.Fprintf(a.Out, "Listening for new Telegram messages for all subscribed profiles\n")
	}
	if pipeline.botToken == "" {
		fmt.Fprintf(a.Out, "Telegram notifications disabled: TELEGRAM_BOT_TOKEN is not set\n")
	}
	nudgeDone := make(chan struct{})
	if pipeline.botToken != "" {
		nudger := quietSearchNudger{
			store: store,
			send: func(ctx context.Context, chatID int64, text string) error {
				_, err := telegram.SendTextMessageID(ctx, pipeline.botToken, chatID, text)
				return err
			},
			onlyProfileID: onlyProfileID,
			errOut:        a.Err,
		}
		go func() {
			defer close(nudgeDone)
			nudger.Run(ctx, quietSearchCheckInterval)
		}()
	} else {
		close(nudgeDone)
	}
	catchUpDone := make(chan struct{})
	go func() {
		defer close(catchUpDone)
		worker.CatchUpNewSubscriptions(ctx)
		worker.RunSubscriptionCatchUp(ctx, subscriptionCatchUpInterval)
	}()
	err = reader.Listen(ctx, worker.HandleLiveMessage)
	cancel()
	<-catchUpDone
	<-nudgeDone
	return err
}

func requireProcessableProfile(ctx context.Context, store *pgstore.Repository, profileID string) error {
	profile, ok, err := store.GetProfile(ctx, profileID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", profileID)
	}
	if profile.Preferences == nil {
		return fmt.Errorf("profile %q has no preferences configured; run /start in the Telegram bot first", profileID)
	}
	if len(profile.Channels) == 0 {
		return fmt.Errorf("profile %q has no telegram channels configured", profileID)
	}
	return nil
}

type listingPipeline struct {
	store                *pgstore.Repository
	reader               *telegramuserbot.Client
	analyzer             evaluation.AnalyzerWithUsage
	geocoder             *googlemaps.Geocoder
	defaultCity          cities.City
	geocodeLog           evaluation.GeocodeLogger
	pipelineLog          evaluation.PipelineLogger
	botToken             string
	mapRenderer          googlemaps.StaticMapRenderer
	richNotifications    bool
	collageNotifications bool
	out                  io.Writer
	errOut               io.Writer
}

func (p listingPipeline) processorFor(profile profiles.Profile) evaluation.Processor {
	city := cities.ForKey(profile.Preferences.City, p.defaultCity)
	processor := evaluation.Processor{
		Prefs:          *profile.Preferences,
		FilterDisabled: profiles.FilterDisabled(profile),
		City:           city,
		Deduplicator:   dedup.NewService(dedup.NewImageHasher(), dedup.ProfileScopedRepository(p.store, profile.ID)),
		Store:          p.store,
		Analyzer:       p.analyzer,
		PhotoLoader:    p.reader,
		GeocodeLog:     p.geocodeLog,
		PipelineLog:    p.pipelineLog,
		Location:       p.store,
	}
	if p.geocoder != nil {
		processor.Geocoder = p.geocoder.ForCity(city)
	}
	return processor
}

func (p listingPipeline) process(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage) error {
	processor := p.processorFor(profile)
	source := domain.ListingSourceRef{
		Source:         "telegram",
		ProfileID:      profile.ID,
		TelegramChatID: profile.TelegramChatID,
	}
	result, err := processor.ProcessMessage(ctx, source, message)
	if err != nil {
		return err
	}
	fmt.Fprintf(p.out, "Processed %s/%d for %s: %s\n", message.ChannelID, message.MessageID, profile.ID, result.Record.Status)
	if p.botToken == "" || profile.TelegramChatID == 0 || !shouldSendTelegramNotification(profile, result.Record) {
		return nil
	}
	return p.notify(ctx, profile, message, result)
}

func (p listingPipeline) notify(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage, result evaluation.Result) error {
	record := result.Record
	fmt.Fprintf(p.out, "Sending notification for %s/%d to chat %d\n", message.ChannelID, message.MessageID, profile.TelegramChatID)
	mapRenderer := p.mapRenderer
	if p.collageNotifications {
		mapRenderer.Size = telegram.ListingCollageMapSize
	}
	mapImage, hasMap, mapErr := listingSearchAreaMapImage(ctx, p.store, mapRenderer, record)
	if mapErr != nil {
		fmt.Fprintf(p.errOut, "Telegram map skipped for %s/%d: %v\n", message.ChannelID, message.MessageID, mapErr)
	}
	if !hasMap {
		mapImage = nil
	}
	format := "collage"
	botMessageID, sent, collageNote, err := p.sendCollageNotification(ctx, profile, message, result, mapImage)
	if err != nil {
		logPipelineEvent(ctx, p.pipelineLog, record, "notification_sent", "failed", err.Error())
		return err
	}
	richNote := ""
	if !sent {
		format = "rich"
		botMessageID, sent, richNote, err = p.sendRichNotification(ctx, profile, message, result, mapImage)
		if err != nil {
			logPipelineEvent(ctx, p.pipelineLog, record, "notification_sent", "failed", err.Error())
			return err
		}
	}
	if !sent {
		format = "plain"
		botMessageID, err = p.sendPlainNotification(ctx, profile, message, record, mapImage)
		if err != nil {
			logPipelineEvent(ctx, p.pipelineLog, record, "notification_sent", "failed", err.Error())
			return err
		}
	}
	if err := p.store.UpdateEvaluationNotificationMessageID(ctx, record.Listing.ID, botMessageID); err != nil {
		logPipelineEvent(ctx, p.pipelineLog, record, "notification_saved", "failed", err.Error())
		return err
	}
	logPipelineEvent(ctx, p.pipelineLog, record, "notification_sent", "sent", strings.TrimSpace(fmt.Sprintf("format=%s map=%t photos=%d %s %s", format, len(mapImage) > 0, len(result.NotificationPhotos), collageNote, richNote)))
	logPipelineEvent(ctx, p.pipelineLog, record, "notification_saved", "saved", "")
	return nil
}

func (p listingPipeline) sendCollageNotification(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage, result evaluation.Result, mapImage []byte) (int, bool, string, error) {
	if !p.collageNotifications {
		return 0, false, "collage_disabled", nil
	}
	botMessageID, err := telegram.SendListingCollageNotification(ctx, p.botToken, profile.TelegramChatID, telegram.ListingCollageNotification{
		Report:   result.ReportView,
		MapImage: mapImage,
		Photos:   result.NotificationPhotos,
		Hint:     listingCollageHint,
	})
	if err == nil {
		return botMessageID, true, "", nil
	}
	if errors.Is(err, telegram.ErrListingCollageRejected) {
		fmt.Fprintf(p.errOut, "Telegram listing collage rejected for %s/%d, falling back: %v\n", message.ChannelID, message.MessageID, err)
		return 0, false, "collage_error=" + err.Error(), nil
	}
	return 0, false, "", err
}

func (p listingPipeline) sendRichNotification(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage, result evaluation.Result, mapImage []byte) (int, bool, string, error) {
	if !p.richNotifications {
		return 0, false, "rich_disabled", nil
	}
	botMessageID, err := telegram.SendListingNotificationMessageID(ctx, p.botToken, profile.TelegramChatID, telegram.ListingNotification{
		Report:    result.ReportView,
		MapImage:  mapImage,
		Photos:    result.NotificationPhotos,
		ReplyHint: strings.TrimSpace(listingReplyHint),
	})
	if err == nil {
		return botMessageID, true, "", nil
	}
	if errors.Is(err, telegram.ErrRichMessageRejected) {
		fmt.Fprintf(p.errOut, "Telegram rich notification rejected for %s/%d, sending plain notification: %v\n", message.ChannelID, message.MessageID, err)
		return 0, false, "rich_error=" + err.Error(), nil
	}
	return 0, false, "", err
}

func (p listingPipeline) sendPlainNotification(ctx context.Context, profile profiles.Profile, message sources.ChannelMessage, record domain.ListingEvaluationRecord, mapImage []byte) (int, error) {
	notificationText := evaluation.NotificationText(record) + listingReplyHint
	if len(mapImage) > 0 {
		if telegramCaptionFits(notificationText) {
			botMessageID, err := telegram.SendPhotoBytesMessageID(ctx, p.botToken, profile.TelegramChatID, "search-area-map.png", mapImage, notificationText)
			if err == nil {
				return botMessageID, nil
			}
			fmt.Fprintf(p.errOut, "Telegram map notification failed for %s/%d: %v\n", message.ChannelID, message.MessageID, err)
		} else if err := telegram.SendPhotoBytes(ctx, p.botToken, profile.TelegramChatID, "search-area-map.png", mapImage, "Гео: квартира и зеленая зона поиска"); err != nil {
			fmt.Fprintf(p.errOut, "Telegram map send failed for %s/%d: %v\n", message.ChannelID, message.MessageID, err)
		}
	}
	return telegram.SendTextMessageID(ctx, p.botToken, profile.TelegramChatID, notificationText)
}

func richNotificationsEnabled(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "false", "0", "no", "off":
		return false
	default:
		return true
	}
}

const listingCollageHint = "💬 Ответь на это сообщение, что нравится или не подходит, и я подстрою поиск."

const listingReplyHint = "\n\n💬 Ответь на это сообщение, что нравится или не подходит, и я подстрою поиск."

func telegramCaptionFits(text string) bool {
	return len(utf16.Encode([]rune(text))) <= telegramPhotoCaptionMaxUnits
}

// albumTextResolver fetches the caption/text of a grouped Telegram album by scanning
// sibling messages that share the same grouped id.
type albumTextResolver interface {
	FetchAlbumText(ctx context.Context, channel sources.ChannelRef, groupedID int64, anchorMessageID int) (string, error)
}

// enrichGroupedAlbumText repairs album fragments that arrive without their caption.
// Telegram puts an album's text/price on a single message of the group; when the group
// is split across fetch/listen batches a photo-only fragment can reach the pipeline with
// empty text, producing price=0 and a default title. In that case we look the text up
// from the album's sibling messages and merge it in.
func enrichGroupedAlbumText(ctx context.Context, resolver albumTextResolver, message sources.ChannelMessage, errOut io.Writer) sources.ChannelMessage {
	if resolver == nil {
		return message
	}
	if message.GroupedID == 0 || strings.TrimSpace(message.Text) != "" {
		return message
	}
	channel := sources.ChannelRef{ID: message.ChannelID, Username: message.ChannelUsername}
	text, err := resolver.FetchAlbumText(ctx, channel, message.GroupedID, message.MessageID)
	if err != nil {
		if errOut != nil {
			fmt.Fprintf(errOut, "album text lookup failed for %s/%d (grouped %d): %v\n", message.ChannelID, message.MessageID, message.GroupedID, err)
		}
		return message
	}
	if strings.TrimSpace(text) != "" {
		message.Text = text
	}
	return message
}

func resolvedChannelSource(fallback sources.ChannelRef, messages []sources.ChannelMessage) sources.ChannelRef {
	resolved := fallback
	for _, message := range messages {
		if strings.TrimSpace(message.ChannelID) != "" {
			resolved.ID = message.ChannelID
		}
		if strings.TrimSpace(message.ChannelUsername) != "" {
			resolved.Username = message.ChannelUsername
		}
		if strings.TrimSpace(message.ChannelName) != "" {
			resolved.Title = message.ChannelName
		}
		if resolved.ID != "" && resolved.Username != "" {
			return resolved
		}
	}
	return resolved
}

func subscriptionName(sub profiles.ChannelSubscription) string {
	if sub.Source.Username != "" {
		return strings.TrimPrefix(sub.Source.Username, "@")
	}
	return strings.TrimPrefix(sub.Source.ID, "@")
}

func shouldSendTelegramNotification(profile profiles.Profile, record domain.ListingEvaluationRecord) bool {
	if record.Status == "rejected_non_listing" {
		return false
	}
	return profiles.DebugModeEnabled(profile) || record.Status == "passed"
}

type latestSearchAreaStore interface {
	LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error)
}

func listingSearchAreaMapURL(ctx context.Context, store latestSearchAreaStore, renderer googlemaps.StaticMapRenderer, record domain.ListingEvaluationRecord) (string, bool, error) {
	if !shouldSendListingSearchAreaMap(record) || store == nil {
		return "", false, nil
	}
	geoJSON, _, err := store.LatestSearchAreaGeoJSON(ctx, record.Source.ProfileID)
	if err != nil {
		return "", false, err
	}
	mapURL := renderer.ListingSearchAreaURL(record.Listing, geoJSON)
	if mapURL == "" {
		return "", false, nil
	}
	return mapURL, true, nil
}

func listingSearchAreaMapImage(ctx context.Context, store latestSearchAreaStore, renderer googlemaps.StaticMapRenderer, record domain.ListingEvaluationRecord) ([]byte, bool, error) {
	if !shouldSendListingSearchAreaMap(record) || store == nil {
		return nil, false, nil
	}
	geoJSON, _, err := store.LatestSearchAreaGeoJSON(ctx, record.Source.ProfileID)
	if err != nil {
		return nil, false, err
	}
	image, err := renderer.ListingSearchAreaImage(ctx, record.Listing, geoJSON)
	if err != nil || len(image) == 0 {
		return nil, false, err
	}
	return image, true, nil
}

func shouldSendListingSearchAreaMap(record domain.ListingEvaluationRecord) bool {
	if record.Listing.Latitude == 0 && record.Listing.Longitude == 0 {
		return false
	}
	switch record.Status {
	case "rejected_basic", "rejected_non_listing", "received", "failed":
		return false
	default:
		return true
	}
}

func profileSubscribedToMessage(profile profiles.Profile, message sources.ChannelMessage) bool {
	for _, sub := range profile.Channels {
		if channelRefMatchesMessage(sub.Source, message) {
			return true
		}
	}
	return false
}

func channelRefMatchesMessage(ref sources.ChannelRef, message sources.ChannelMessage) bool {
	refID := normalizeTelegramChannelKey(ref.ID)
	refUsername := normalizeTelegramChannelKey(ref.Username)
	messageID := normalizeTelegramChannelKey(message.ChannelID)
	messageUsername := normalizeTelegramChannelKey(message.ChannelUsername)
	if refID != "" && (refID == messageID || refID == messageUsername) {
		return true
	}
	if refUsername != "" && (refUsername == messageID || refUsername == messageUsername) {
		return true
	}
	return false
}

func normalizeTelegramChannelKey(value string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(value), "@"))
}

func (a App) telegramRead(profileID string) error {
	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := openPostgresStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()
	profile, ok, err := store.GetProfile(ctx, profileID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("profile %q not found", profileID)
	}
	if len(profile.Channels) == 0 {
		return fmt.Errorf("profile %q has no telegram channels configured", profileID)
	}

	encoder := json.NewEncoder(a.Out)
	encoder.SetIndent("", "  ")
	for _, sub := range profile.Channels {
		messages, cursor, err := reader.Fetch(ctx, sub.Source, sub.Cursor, sub.Limit)
		if err != nil {
			return err
		}
		if err := store.UpdateProfileChannelCursor(ctx, profileID, sub.Source, cursor); err != nil {
			return err
		}
		for _, message := range messages {
			if strings.TrimSpace(message.Text) == "" && len(message.Photos) == 0 {
				continue
			}
			output := struct {
				ProfileID string                  `json:"profile_id"`
				Source    string                  `json:"source"`
				Link      string                  `json:"link,omitempty"`
				Listing   domain.ApartmentListing `json:"listing"`
			}{
				ProfileID: profileID,
				Source:    message.ChannelID,
				Link:      message.Link,
				Listing:   geocodeListingPreview(ctx, listingparser.FromChannelMessage(message), newGoogleGeocoder()),
			}
			if err := encoder.Encode(output); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a App) score(listingPath, prefsPath string) error {
	listingBytes, err := os.ReadFile(listingPath)
	if err != nil {
		return err
	}
	var listing domain.ApartmentListing
	if err := json.Unmarshal(listingBytes, &listing); err != nil {
		return err
	}

	prefs, err := preferences.Load(prefsPath)
	if err != nil {
		return err
	}
	listing = geocodeListingPreview(context.Background(), listing, newGoogleGeocoder())

	evaluator, err := newVisionEvaluator()
	if err != nil {
		return err
	}
	result := scoring.New(evaluator).Score(listing, prefs)
	encoder := json.NewEncoder(a.Out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func newVisionEvaluator() (scoring.InsightsEvaluator, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_VISION"))) {
	case "", "mock":
		return visionmock.New(), nil
	case "openai":
		return openaivision.FromEnv()
	default:
		return nil, fmt.Errorf("unsupported APARTMENT_HUNTER_VISION value %q", os.Getenv("APARTMENT_HUNTER_VISION"))
	}
}

func newLocationResolver() geocoding.Resolver {
	google := newGoogleGeocoder()
	if google == nil {
		return geocodingmock.New()
	}
	return geocoding.RequiredChain{google, geocodingmock.NewKnown()}
}

func parseChatIDs(value string) []int64 {
	var ids []int64
	for _, part := range strings.Split(value, ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64); err == nil && id > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func newGoogleGeocoder() *googlemaps.Geocoder {
	apiKey := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GOOGLE_MAPS_API_KEY"))
	if apiKey == "" {
		return nil
	}
	region := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GEOCODING_REGION"))
	if region == "" {
		region = "Batumi, Georgia"
	}
	geocoder := googlemaps.NewGeocoder(apiKey, region)
	if language := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GEOCODING_LANGUAGE")); language != "" {
		geocoder.Language = language
	}
	return geocoder
}

func newGeocodeLogger(out io.Writer) *evaluation.FileGeocodeLogger {
	dir := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GEOCODING_LOG_DIR"))
	if dir == "" {
		dir = ".logs/geocoding"
	}
	retentionDays := 3
	if raw := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_GEOCODING_LOG_RETENTION_DAYS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 {
			retentionDays = parsed
		}
	}
	return &evaluation.FileGeocodeLogger{
		Dir:       dir,
		Retention: time.Duration(retentionDays) * 24 * time.Hour,
		Output:    jsonLogOutput(out),
	}
}

func newPipelineLogger(out io.Writer) *evaluation.FilePipelineLogger {
	dir := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_PIPELINE_LOG_DIR"))
	if dir == "" {
		dir = ".logs/pipeline"
	}
	retentionDays := 3
	if raw := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_PIPELINE_LOG_RETENTION_DAYS")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err == nil && parsed > 0 {
			retentionDays = parsed
		}
	}
	return &evaluation.FilePipelineLogger{
		Dir:       dir,
		Retention: time.Duration(retentionDays) * 24 * time.Hour,
		Output:    jsonLogOutput(out),
	}
}

func jsonLogOutput(out io.Writer) io.Writer {
	if envBool("APARTMENT_HUNTER_LOG_TO_STDOUT") {
		return out
	}
	return nil
}

func envBool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func logPipelineEvent(ctx context.Context, logger evaluation.PipelineLogger, record domain.ListingEvaluationRecord, stage, status, reason string) {
	if logger == nil {
		return
	}
	_ = logger.LogPipelineEvent(ctx, evaluation.PipelineLogEvent{
		Time:         time.Now().UTC(),
		Source:       record.Source,
		ListingID:    record.Listing.ID,
		ListingTitle: record.Listing.Title,
		Stage:        stage,
		Status:       status,
		Reason:       strings.TrimSpace(reason),
		Price:        record.Listing.Price,
		AreaM2:       record.Listing.AreaM2,
		Latitude:     record.Listing.Latitude,
		Longitude:    record.Listing.Longitude,
		PhotoCount:   len(record.Listing.Photos),
	})
}

func geocodeListingPreview(ctx context.Context, listing domain.ApartmentListing, resolver geocoding.Resolver) domain.ApartmentListing {
	if resolver == nil || listing.Latitude != 0 || listing.Longitude != 0 {
		return listing
	}
	query := strings.TrimSpace(listing.Description)
	if query == "" {
		query = listing.Title
	}
	loc, err := resolver.Resolve(ctx, query, 0)
	if err != nil || loc.Latitude == 0 || loc.Longitude == 0 {
		return listing
	}
	listing.Latitude = loc.Latitude
	listing.Longitude = loc.Longitude
	if listing.District == "" {
		listing.District = loc.Name
	}
	return listing
}

func openPostgresStore(ctx context.Context) (*pgstore.Repository, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	return pgstore.Open(ctx, databaseURL)
}

func profileIDArg(args []string, command string) (string, error) {
	if len(args) == 2 {
		return args[1], nil
	}
	if len(args) == 3 {
		return args[2], nil
	}
	return "", fmt.Errorf("usage: apartment-hunter %s profile_id", command)
}

func (a App) printHelp() error {
	_, err := fmt.Fprintln(a.Out, `Usage:
  apartment-hunter interview [preferences.yaml]
  apartment-hunter score sample_listing.json preferences.yaml
  DATABASE_URL=... TELEGRAM_BOT_TOKEN=... apartment-hunter telegram
  DATABASE_URL=... TELEGRAM_API_ID=... TELEGRAM_API_HASH=... TELEGRAM_PHONE=... apartment-hunter telegram-read profile_id
  DATABASE_URL=... TELEGRAM_API_ID=... TELEGRAM_API_HASH=... TELEGRAM_PHONE=... apartment-hunter telegram-process [profile_id]`)
	return err
}
