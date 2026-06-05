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
	"apartment-hunter/internal/preferences"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/scoring"
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
		fmt.Fprintf(a.Out, "Telegram bot started. Saving profiles to Postgres\n")
		return telegram.New(token, outputDir, telegram.WithLocationResolver(newLocationResolver()), telegram.WithProfileStore(store)).Run(ctx.Done())
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
	}

	botToken := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	pollInterval := envDuration("APARTMENT_HUNTER_POLL_INTERVAL", 0)
	for {
		profile, ok, err = store.GetProfile(ctx, profileID)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("profile %q not found", profileID)
		}
		if profile.Preferences == nil {
			return fmt.Errorf("profile %q has no preferences configured; run /start in the Telegram bot first", profileID)
		}
		processor.Prefs = *profile.Preferences
		processed, sent, err := a.processProfileOnce(ctx, store, profileID, profile, reader, processor, botToken)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Out, "Processed %d messages, sent %d matches\n", processed, sent)
		if pollInterval <= 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

func (a App) processProfileOnce(ctx context.Context, store *pgstore.Repository, profileID string, profile profiles.Profile, reader *telegramuserbot.Client, processor evaluation.Processor, botToken string) (int, int, error) {
	processed := 0
	sent := 0
	for _, sub := range profile.Channels {
		messages, cursor, err := reader.Fetch(ctx, sub.Source, sub.Cursor, sub.Limit)
		if err != nil {
			return processed, sent, err
		}
		if err := store.UpdateProfileChannelCursor(ctx, profileID, sub.Source, cursor); err != nil {
			return processed, sent, err
		}
		for _, message := range messages {
			if strings.TrimSpace(message.Text) == "" && len(message.Photos) == 0 {
				continue
			}
			source := domain.ListingSourceRef{
				Source:         "telegram",
				ProfileID:      profileID,
				TelegramChatID: profile.TelegramChatID,
			}
			result, err := processor.ProcessMessage(ctx, source, message)
			if err != nil {
				return processed, sent, err
			}
			processed++
			if result.Record.Status == "passed" && botToken != "" && profile.TelegramChatID != 0 {
				if err := telegram.SendText(ctx, botToken, profile.TelegramChatID, evaluation.NotificationText(result.Record)); err != nil {
					return processed, sent, err
				}
				sent++
			}
		}
	}
	return processed, sent, nil
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

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
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
