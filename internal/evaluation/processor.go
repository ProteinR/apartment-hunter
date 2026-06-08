package evaluation

import (
	"context"
	"fmt"
	"os"
	"regexp"
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

type LocationMatcher interface {
	MatchesListingLocation(ctx context.Context, profileID string, listing domain.ApartmentListing) (bool, string, error)
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
	Location     LocationMatcher
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

	if !looksLikeApartmentListing(message, listing) {
		record.Score = &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  []string{"message is not an apartment listing"},
			ScoreTotal:        0,
			ScoreBreakdown:    scoring.BasicScoreBreakdown(listing, p.Prefs),
			ShortSummary:      listing.Title + " rejected: message is not an apartment listing.",
		}
		record.Status = "rejected_non_listing"
		return p.finish(ctx, record)
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

	if p.Location != nil {
		ok, reason, err := p.Location.MatchesListingLocation(ctx, source.ProfileID, listing)
		if err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			return p.finish(ctx, record)
		}
		if !ok {
			if strings.TrimSpace(reason) == "" {
				reason = "listing location is outside preferred search area"
			}
			record.Score = &domain.ListingScore{
				PassedHardFilters: false,
				RejectionReasons:  []string{reason},
				ScoreTotal:        0,
				ScoreBreakdown:    scoring.BasicScoreBreakdown(listing, p.Prefs),
				ShortSummary:      listing.Title + " rejected: " + reason + ".",
			}
			record.Status = "rejected_location"
			return p.finish(ctx, record)
		}
	}

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
	listing = scoringListingWithInsights(listing, insights)
	record.Listing = listing
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

func scoringListingWithInsights(listing domain.ApartmentListing, insights domain.ListingInsights) domain.ApartmentListing {
	if insights.HasDishwasher {
		listing.HasDishwasher = true
	}
	if insights.HasOven {
		listing.HasOven = true
	}
	return listing
}

func looksLikeApartmentListing(message sources.ChannelMessage, listing domain.ApartmentListing) bool {
	if len(message.Photos) > 0 || listing.Price > 0 || listing.AreaM2 > 0 {
		return true
	}

	text := strings.ToLower(strings.TrimSpace(listing.Title + "\n" + listing.Description))
	if text == "" {
		return len(message.Photos) > 0
	}

	serviceSignals := []string{
		"абонемент",
		"администратор",
		"активаци",
		"закрепл",
		"оплат",
		"правила",
		"публикаци",
		"размещени",
		"реклам",
		"welcome to the group",
		"добро пожаловать в группу",
	}
	propertySignals := []string{
		"апартамент",
		"аренд",
		"bedroom",
		"flat",
		"rent",
		"sale",
		"studio",
		"квар",
		"прода",
		"сдам",
		"сдаю",
		"сдаёт",
		"сдается",
		"сдаётся",
		"студи",
	}

	hasServiceSignal := containsAnySubstring(text, serviceSignals...)
	hasPropertySignal := containsAnySubstring(text, propertySignals...)
	if hasServiceSignal && !hasPropertySignal {
		return false
	}
	if strings.Contains(text, "групп") && strings.Contains(text, "недвижимост") && !hasPropertySignal {
		return false
	}
	return hasPropertySignal
}

func containsAnySubstring(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func (p Processor) geocodeListing(ctx context.Context, listing domain.ApartmentListing) domain.ApartmentListing {
	if p.Geocoder == nil || listing.Latitude != 0 || listing.Longitude != 0 {
		return listing
	}
	for _, candidate := range geocodeListingQueries(listing) {
		loc, err := p.Geocoder.Resolve(ctx, candidate.Query, candidate.MaxDistanceMeters)
		if err != nil || loc.Latitude == 0 || loc.Longitude == 0 {
			continue
		}
		listing.Latitude = loc.Latitude
		listing.Longitude = loc.Longitude
		if listing.District == "" {
			listing.District = loc.Name
		}
		return listing
	}
	return listing
}

var addressLinePattern = regexp.MustCompile(`(?i)(^|[\s,])(?:ул\.?|улиц[аеуы]?|street|st\.?|avenue|ave\.?|проспект|пр-т|переулок|пер\.?|road|rd\.?|lane|ln\.?)\s+`)
var addressLabelPattern = regexp.MustCompile(`(?i)^\s*(?:address|адрес)\s*:\s*`)
var streetCasePattern = regexp.MustCompile(`(?i)^улиц[еуы]`)

const listingAddressSearchRadiusMeters = 15000

type geocodeListingQuery struct {
	Query             string
	MaxDistanceMeters int
}

func geocodeListingQueries(listing domain.ApartmentListing) []geocodeListingQuery {
	var queries []geocodeListingQuery
	seen := map[string]bool{}
	addressFound := false
	add := func(query string, maxDistanceMeters int) {
		query = cleanGeocodeQuery(query)
		if query == "" || seen[query] {
			return
		}
		seen[query] = true
		queries = append(queries, geocodeListingQuery{Query: query, MaxDistanceMeters: maxDistanceMeters})
	}
	addAddress := func(query string) {
		query = cleanGeocodeQuery(query)
		if query == "" {
			return
		}
		addressFound = true
		add(query+", Batumi, Georgia", listingAddressSearchRadiusMeters)
	}

	for _, line := range strings.Split(listing.Description, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "📍") {
			addAddress(strings.TrimSpace(line[strings.LastIndex(line, "📍")+len("📍"):]))
			continue
		}
		if address := addressFromLine(line); address != "" {
			addAddress(address)
		}
	}
	if !addressFound {
		add(listing.Description, 0)
		add(listing.Title, 0)
	}
	return queries
}

func addressFromLine(line string) string {
	match := addressLinePattern.FindStringIndex(line)
	if match == nil {
		return ""
	}
	address := strings.TrimSpace(line[match[0]:])
	address = streetCasePattern.ReplaceAllString(address, "улица")
	return address
}

func cleanGeocodeQuery(query string) string {
	query = strings.TrimSpace(query)
	query = strings.TrimLeft(query, " \t-–—•*✔️✅🔶⚜️")
	query = addressLabelPattern.ReplaceAllString(query, "")
	query = strings.TrimSpace(query)
	return query
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
