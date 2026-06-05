package evaluation

import (
	"context"
	"fmt"
	"os"
	"strings"

	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/listingparser"
	"apartment-hunter/internal/scoring"
	"apartment-hunter/internal/sources"
)

type AnalyzerWithUsage interface {
	AnalyzeWithUsage(ctx context.Context, listing domain.ApartmentListing) (domain.ListingInsights, domain.AIUsage, error)
}

type HistoryStore interface {
	SaveEvaluation(ctx context.Context, record domain.ListingEvaluationRecord) error
}

type PhotoLoader interface {
	LoadPhotos(ctx context.Context, message sources.ChannelMessage, listing domain.ApartmentListing) (domain.ApartmentListing, error)
}

type Processor struct {
	Prefs        domain.UserPreferences
	Deduplicator dedup.ListingDeduplicator
	Store        HistoryStore
	Analyzer     AnalyzerWithUsage
	PhotoLoader  PhotoLoader
	Geocoder     ListingGeocoder
}

type ListingGeocoder interface {
	Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error)
}

type Result struct {
	Record domain.ListingEvaluationRecord
}

func (p Processor) ProcessMessage(ctx context.Context, source domain.ListingSourceRef, message sources.ChannelMessage) (Result, error) {
	listing := listingparser.FromChannelMessage(message)
	source.ChannelID = message.ChannelID
	source.ChannelName = message.ChannelName
	source.MessageID = message.MessageID
	source.MessageLink = message.Link
	source.GroupedID = message.GroupedID

	record := domain.ListingEvaluationRecord{
		Listing: listing,
		Source:  source,
		Status:  "received",
	}

	basicReasons := scoring.BasicFilterReasons(listing, p.Prefs)
	if len(basicReasons) > 0 {
		record.Score = &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  basicReasons,
			ScoreTotal:        0,
			ScoreBreakdown:    scoring.BasicScoreBreakdown(listing, p.Prefs),
			ShortSummary:      listing.Title + " rejected: " + strings.Join(basicReasons, "; ") + ".",
		}
		record.Status = "rejected_basic"
		return p.finish(ctx, record)
	}

	listing = p.geocodeListing(ctx, listing)
	record.Listing = listing

	if p.PhotoLoader != nil && len(message.Photos) > 0 {
		loaded, err := p.PhotoLoader.LoadPhotos(ctx, message, listing)
		if err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			return p.finish(ctx, record)
		}
		listing = loaded
		record.Listing = listing
	}
	defer cleanupLocalPhotoFiles(listing.Photos)

	hashable := listingWithHashablePhotos(listing)
	if p.Deduplicator != nil && len(hashable.Photos) > 0 {
		duplicate, err := p.Deduplicator.CheckDuplicate(ctx, hashable)
		if err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			return p.finish(ctx, record)
		}
		if duplicate != nil {
			record.Duplicate = duplicate
			record.Status = "duplicate"
			return p.finish(ctx, record)
		}
	}

	if p.Analyzer == nil {
		record.Status = "failed"
		record.FailureReason = "AI analyzer is not configured"
		return p.finish(ctx, record)
	}

	insights, usage, err := p.Analyzer.AnalyzeWithUsage(ctx, listing)
	if err != nil {
		record.Status = "failed"
		record.FailureReason = err.Error()
		record.AIUsage = &usage
		return p.finish(ctx, record)
	}
	record.Insights = &insights
	record.AIUsage = &usage
	score := scoring.ScoreWithInsights(listing, p.Prefs, insights)
	record.Score = &score
	if score.PassedHardFilters {
		record.Status = "passed"
	} else {
		record.Status = "rejected_ai"
	}

	if p.Deduplicator != nil && len(hashable.Photos) > 0 {
		if err := p.Deduplicator.SaveFingerprint(ctx, listing.ID, hashable.Photos); err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			return p.finish(ctx, record)
		}
	}
	return p.finish(ctx, record)
}

func (p Processor) geocodeListing(ctx context.Context, listing domain.ApartmentListing) domain.ApartmentListing {
	if p.Geocoder == nil || listing.Latitude != 0 || listing.Longitude != 0 {
		return listing
	}
	query := strings.TrimSpace(listing.Description)
	if query == "" {
		query = listing.Title
	}
	if query == "" {
		return listing
	}
	loc, err := p.Geocoder.Resolve(ctx, query, 0)
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

func listingWithHashablePhotos(listing domain.ApartmentListing) domain.ApartmentListing {
	hashable := listing
	hashable.Photos = nil
	for _, photo := range listing.Photos {
		if strings.HasPrefix(photo.URL, "http://") ||
			strings.HasPrefix(photo.URL, "https://") ||
			strings.HasPrefix(photo.URL, "file://") {
			hashable.Photos = append(hashable.Photos, photo)
		}
	}
	return hashable
}

func cleanupLocalPhotoFiles(photos []domain.ApartmentPhoto) {
	for _, photo := range photos {
		if !strings.HasPrefix(photo.URL, "file://") {
			continue
		}
		_ = os.Remove(strings.TrimPrefix(photo.URL, "file://"))
	}
}

func (p Processor) finish(ctx context.Context, record domain.ListingEvaluationRecord) (Result, error) {
	record.Listing = listingWithoutLocalPhotoFiles(record.Listing)
	record.Report = BuildReport(record, p.Prefs)
	return Result{Record: record}, p.save(ctx, record)
}

func listingWithoutLocalPhotoFiles(listing domain.ApartmentListing) domain.ApartmentListing {
	for i := range listing.Photos {
		if strings.HasPrefix(listing.Photos[i].URL, "file://") {
			listing.Photos[i].URL = "local-photo-redacted"
		}
	}
	return listing
}

func (p Processor) save(ctx context.Context, record domain.ListingEvaluationRecord) error {
	if p.Store == nil {
		return nil
	}
	return p.Store.SaveEvaluation(ctx, record)
}

func NotificationText(record domain.ListingEvaluationRecord) string {
	if strings.TrimSpace(record.Report) != "" {
		return record.Report
	}
	score := 0.0
	if record.Score != nil {
		score = record.Score.ScoreTotal
	}
	lines := []string{
		fmt.Sprintf("%s", record.Listing.Title),
		fmt.Sprintf("Score: %.2f/100", score),
	}
	if record.Listing.Price > 0 {
		lines = append(lines, fmt.Sprintf("Price: %d", record.Listing.Price))
	}
	if record.Listing.AreaM2 > 0 {
		lines = append(lines, fmt.Sprintf("Area: %.1f m2", record.Listing.AreaM2))
	}
	if record.Source.MessageLink != "" {
		lines = append(lines, record.Source.MessageLink)
	}
	if record.Score != nil && record.Score.ShortSummary != "" {
		lines = append(lines, record.Score.ShortSummary)
	}
	return strings.Join(lines, "\n")
}
