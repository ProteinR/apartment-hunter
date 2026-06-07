package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"apartment-hunter/internal/adapters/geocodingmock"
	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/adapters/openaipreferences"
	"apartment-hunter/internal/adapters/openaivision"
	"apartment-hunter/internal/adapters/telegram"
	"apartment-hunter/internal/adapters/telegramuserbot"
	"apartment-hunter/internal/adapters/visionmock"
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
		profileID, err := profileIDArg(args, "telegram-process")
		if err != nil {
			return err
		}
		return a.telegramProcess(profileID)
	case "help", "-h", "--help":
		return a.printHelp()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a App) telegramProcess(profileID string) error {
	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return a.telegramProcessWithContext(ctx, profileID, reader)
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

	if profileID := strings.TrimSpace(os.Getenv("APARTMENT_HUNTER_PROFILE_ID")); profileID != "" {
		if reason := telegramWorkerDisabledReason(); reason != "" {
			fmt.Fprintf(a.Out, "Telegram worker not started: %s\n", reason)
		} else {
			go func() {
				if err := a.runTelegramWorker(ctx, profileID); err != nil {
					fmt.Fprintf(a.Err, "Telegram worker failed for %s: %v\n", profileID, err)
				}
			}()
		}
	} else {
		fmt.Fprintf(a.Out, "Telegram worker not started: APARTMENT_HUNTER_PROFILE_ID is not set\n")
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
	fmt.Fprintf(a.Out, "Telegram bot started. Saving profiles to Postgres\n")

	err = telegram.New(token, outputDir, telegram.WithPreferencesParser(prefsParser), telegram.WithGeometryBuilder(geometry), telegram.WithProfileStore(store)).Run(ctx.Done())
	stop()
	return err
}

func (a App) runTelegramWorker(ctx context.Context, profileID string) error {
	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}

	for {
		err := a.telegramProcessWithContext(ctx, profileID, reader)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil {
			return nil
		}
		fmt.Fprintf(a.Err, "Telegram worker stopped for %s: %v; retrying in 15s\n", profileID, err)
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

func (a App) telegramProcessWithContext(ctx context.Context, profileID string, reader *telegramuserbot.Client) error {
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
	if profile.Preferences == nil {
		return fmt.Errorf("profile %q has no preferences configured; run /start in the Telegram bot first", profileID)
	}
	if len(profile.Channels) == 0 {
		return fmt.Errorf("profile %q has no telegram channels configured", profileID)
	}

	analyzer, err := openaivision.FromEnv()
	if err != nil {
		return err
	}
	processor := evaluation.Processor{
		Prefs:        *profile.Preferences,
		Deduplicator: dedup.NewService(dedup.NewImageHasher(), store),
		Store:        store,
		Analyzer:     analyzer,
		PhotoLoader:  reader,
		Geocoder:     newGoogleGeocoder(),
		Location:     store,
	}

	botToken := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	fmt.Fprintf(a.Out, "Listening for new Telegram messages for %s\n", profileID)
	if botToken == "" || profile.TelegramChatID == 0 {
		fmt.Fprintf(a.Out, "Telegram notifications disabled: bot token set=%t, chat id=%d\n", botToken != "", profile.TelegramChatID)
	} else {
		fmt.Fprintf(a.Out, "Telegram notifications enabled for chat %d, debug mode=%t\n", profile.TelegramChatID, profiles.DebugModeEnabled(profile))
	}
	return reader.Listen(ctx, func(ctx context.Context, message sources.ChannelMessage) error {
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
		if !profileSubscribedToMessage(profile, message) {
			return nil
		}
		if strings.TrimSpace(message.Text) == "" && len(message.Photos) == 0 {
			return nil
		}
		processor.Prefs = *profile.Preferences
		source := domain.ListingSourceRef{
			Source:         "telegram",
			ProfileID:      profileID,
			TelegramChatID: profile.TelegramChatID,
		}
		result, err := processor.ProcessMessage(ctx, source, message)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Processed %s/%d: %s\n", message.ChannelID, message.MessageID, result.Record.Status)
		if botToken != "" && profile.TelegramChatID != 0 && shouldSendTelegramNotification(profile, result.Record) {
			fmt.Fprintf(a.Out, "Sending notification for %s/%d to chat %d\n", message.ChannelID, message.MessageID, profile.TelegramChatID)
			if err := telegram.SendText(ctx, botToken, profile.TelegramChatID, evaluation.NotificationText(result.Record)); err != nil {
				return err
			}
		}
		return nil
	})
}

func shouldSendTelegramNotification(profile profiles.Profile, record domain.ListingEvaluationRecord) bool {
	return profiles.DebugModeEnabled(profile) || record.Status == "passed"
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
  DATABASE_URL=... TELEGRAM_API_ID=... TELEGRAM_API_HASH=... TELEGRAM_PHONE=... apartment-hunter telegram-process profile_id`)
	return err
}
