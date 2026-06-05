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

	"apartment-hunter/internal/adapters/openaivision"
	"apartment-hunter/internal/adapters/telegram"
	"apartment-hunter/internal/adapters/telegramuserbot"
	"apartment-hunter/internal/adapters/visionmock"
	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/evaluation"
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
		prefs, err := interview.New(a.In, a.Out).Run()
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
		fmt.Fprintf(a.Out, "Telegram bot started. Saving preferences to %s\n", outputDir)
		return telegram.New(token, outputDir).Run(ctx.Done())
	case "telegram-read":
		if len(args) != 3 {
			return fmt.Errorf("usage: apartment-hunter telegram-read profiles.json profile_id")
		}
		return a.telegramRead(args[1], args[2])
	case "telegram-process":
		if len(args) != 3 {
			return fmt.Errorf("usage: apartment-hunter telegram-process profiles.json profile_id")
		}
		return a.telegramProcess(args[1], args[2])
	case "help", "-h", "--help":
		return a.printHelp()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func (a App) telegramProcess(profilesPath, profileID string) error {
	cfg, err := profiles.Load(profilesPath)
	if err != nil {
		return err
	}
	profile, ok := cfg.Find(profileID)
	if !ok {
		return fmt.Errorf("profile %q not found", profileID)
	}
	if len(profile.Channels) == 0 {
		return fmt.Errorf("profile %q has no telegram channels configured", profileID)
	}
	prefs, err := preferences.Load(profile.PreferencesPath)
	if err != nil {
		return err
	}
	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := pgstore.Open(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer store.Close()

	analyzer, err := openaivision.FromEnv()
	if err != nil {
		return err
	}
	processor := evaluation.Processor{
		Prefs:        prefs,
		Deduplicator: dedup.NewService(dedup.NewImageHasher(), store),
		Store:        store,
		Analyzer:     analyzer,
		PhotoLoader:  reader,
	}

	botToken := strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	pollInterval := envDuration("APARTMENT_HUNTER_POLL_INTERVAL", 0)
	for {
		processed, sent, err := a.processProfileOnce(ctx, &cfg, profilesPath, profileID, profile, reader, processor, botToken)
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

func (a App) processProfileOnce(ctx context.Context, cfg *profiles.Config, profilesPath, profileID string, profile profiles.Profile, reader *telegramuserbot.Client, processor evaluation.Processor, botToken string) (int, int, error) {
	processed := 0
	sent := 0
	for profileIndex := range cfg.Profiles {
		if cfg.Profiles[profileIndex].ID != profileID {
			continue
		}
		for channelIndex := range cfg.Profiles[profileIndex].Channels {
			sub := &cfg.Profiles[profileIndex].Channels[channelIndex]
			messages, cursor, err := reader.Fetch(ctx, sub.Source, sub.Cursor, sub.Limit)
			if err != nil {
				return processed, sent, err
			}
			sub.Cursor = cursor
			if err := profiles.Save(profilesPath, *cfg); err != nil {
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
		break
	}
	return processed, sent, nil
}

func (a App) telegramRead(profilesPath, profileID string) error {
	cfg, err := profiles.Load(profilesPath)
	if err != nil {
		return err
	}
	profile, ok := cfg.Find(profileID)
	if !ok {
		return fmt.Errorf("profile %q not found", profileID)
	}
	if len(profile.Channels) == 0 {
		return fmt.Errorf("profile %q has no telegram channels configured", profileID)
	}

	reader, err := telegramuserbot.FromEnv(a.In)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	encoder := json.NewEncoder(a.Out)
	encoder.SetIndent("", "  ")
	for profileIndex := range cfg.Profiles {
		if cfg.Profiles[profileIndex].ID != profileID {
			continue
		}
		for channelIndex := range cfg.Profiles[profileIndex].Channels {
			sub := &cfg.Profiles[profileIndex].Channels[channelIndex]
			messages, cursor, err := reader.Fetch(ctx, sub.Source, sub.Cursor, sub.Limit)
			if err != nil {
				return err
			}
			sub.Cursor = cursor
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
					Listing:   listingparser.FromChannelMessage(message),
				}
				if err := encoder.Encode(output); err != nil {
					return err
				}
			}
		}
		break
	}
	return profiles.Save(profilesPath, cfg)
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

func (a App) printHelp() error {
	_, err := fmt.Fprintln(a.Out, `Usage:
  apartment-hunter interview [preferences.yaml]
  apartment-hunter score sample_listing.json preferences.yaml
  TELEGRAM_BOT_TOKEN=... apartment-hunter telegram [output_dir]
  TELEGRAM_API_ID=... TELEGRAM_API_HASH=... TELEGRAM_PHONE=... apartment-hunter telegram-read profiles.json profile_id
  DATABASE_URL=... TELEGRAM_API_ID=... TELEGRAM_API_HASH=... TELEGRAM_PHONE=... apartment-hunter telegram-process profiles.json profile_id`)
	return err
}
