package evaluation

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"apartment-hunter/internal/cities"
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

type GeocodeLogger interface {
	LogGeocodeAttempt(ctx context.Context, event GeocodeLogEvent) error
}

type PipelineLogger interface {
	LogPipelineEvent(ctx context.Context, event PipelineLogEvent) error
}

type GeocodeLogEvent struct {
	Time              time.Time               `json:"time"`
	Source            domain.ListingSourceRef `json:"source"`
	ListingID         string                  `json:"listing_id,omitempty"`
	ListingTitle      string                  `json:"listing_title,omitempty"`
	Query             string                  `json:"query"`
	MaxDistanceMeters int                     `json:"max_distance_meters,omitempty"`
	ResultName        string                  `json:"result_name,omitempty"`
	ResultLatitude    float64                 `json:"result_latitude,omitempty"`
	ResultLongitude   float64                 `json:"result_longitude,omitempty"`
	Success           bool                    `json:"success"`
	Error             string                  `json:"error,omitempty"`
}

type PipelineLogEvent struct {
	Time               time.Time               `json:"time"`
	Source             domain.ListingSourceRef `json:"source"`
	ListingID          string                  `json:"listing_id,omitempty"`
	ListingTitle       string                  `json:"listing_title,omitempty"`
	Stage              string                  `json:"stage"`
	Status             string                  `json:"status,omitempty"`
	Reason             string                  `json:"reason,omitempty"`
	Price              int                     `json:"price,omitempty"`
	AreaM2             float64                 `json:"area_m2,omitempty"`
	Latitude           float64                 `json:"latitude,omitempty"`
	Longitude          float64                 `json:"longitude,omitempty"`
	PhotoCount         int                     `json:"photo_count,omitempty"`
	HashablePhotoCount int                     `json:"hashable_photo_count,omitempty"`
	DurationMS         int64                   `json:"duration_ms,omitempty"`
	Error              string                  `json:"error,omitempty"`
}

type Processor struct {
	Prefs          domain.UserPreferences
	Deduplicator   dedup.ListingDeduplicator
	Store          HistoryStore
	Analyzer       AnalyzerWithUsage
	PhotoLoader    PhotoLoader
	Geocoder       ListingGeocoder
	GeocodeLog     GeocodeLogger
	PipelineLog    PipelineLogger
	Location       LocationMatcher
	FilterDisabled bool
	City           cities.City
}

type ListingGeocoder interface {
	Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error)
}

type geocodeRequestQueryFormatter interface {
	RequestQuery(query string) string
}

type Result struct {
	Record domain.ListingEvaluationRecord
}

func (p Processor) ProcessMessage(ctx context.Context, source domain.ListingSourceRef, message sources.ChannelMessage) (Result, error) {
	started := time.Now()
	listing := listingparser.FromChannelMessage(message)
	listing.ID = ProfileScopedListingID(source.ProfileID, listing.ID)
	source.ChannelID = message.ChannelID
	source.ChannelName = message.ChannelName
	source.ChannelUsername = message.ChannelUsername
	source.MessageID = message.MessageID
	source.MessageLink = message.Link
	source.GroupedID = message.GroupedID

	record := domain.ListingEvaluationRecord{
		Listing: listing,
		Source:  source,
		Status:  "received",
	}
	p.logPipeline(ctx, pipelineEvent(started, source, listing, "received", "received", ""))
	p.logPipeline(ctx, pipelineEvent(started, source, listing, "parsed", "parsed", ""))

	if !looksLikeApartmentListing(message, listing) {
		record.Score = &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  []string{"message is not an apartment listing"},
			ScoreTotal:        0,
			ScoreBreakdown:    scoring.BasicScoreBreakdown(listing, p.Prefs),
			ShortSummary:      listing.Title + " rejected: message is not an apartment listing.",
		}
		record.Status = "rejected_non_listing"
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "non_listing_filter", record.Status, "message is not an apartment listing"))
		return p.finish(ctx, started, record)
	}
	p.logPipeline(ctx, pipelineEvent(started, source, listing, "non_listing_filter", "passed", ""))

	if !p.FilterDisabled {
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
			p.logPipeline(ctx, pipelineEvent(started, source, listing, "basic_filter", record.Status, strings.Join(basicReasons, "; ")))
			return p.finish(ctx, started, record)
		}
	}
	p.logPipeline(ctx, pipelineEvent(started, source, listing, "basic_filter", "passed", ""))

	listing = p.geocodeListing(ctx, source, listing)
	record.Listing = listing
	if listing.Latitude == 0 && listing.Longitude == 0 {
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "geocoded", "unresolved", "listing location is unresolved"))
	} else {
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "geocoded", "resolved", listing.District))
	}

	if p.Location != nil && !p.FilterDisabled {
		ok, reason, err := p.Location.MatchesListingLocation(ctx, source.ProfileID, listing)
		if err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			p.logPipeline(ctx, pipelineEventWithError(started, source, listing, "location_checked", record.Status, "", err))
			return p.finish(ctx, started, record)
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
			p.logPipeline(ctx, pipelineEvent(started, source, listing, "location_checked", record.Status, reason))
			return p.finish(ctx, started, record)
		}
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "location_checked", "passed", ""))
	} else if p.Location != nil {
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "location_checked", "passed_disabled", ""))
	}

	if p.PhotoLoader != nil && len(message.Photos) > 0 {
		loaded, err := p.PhotoLoader.LoadPhotos(ctx, message, listing)
		if err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			p.logPipeline(ctx, pipelineEventWithError(started, source, listing, "photos_loaded", record.Status, "", err))
			return p.finish(ctx, started, record)
		}
		listing = loaded
		record.Listing = listing
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "photos_loaded", "loaded", ""))
	}
	defer cleanupLocalPhotoFiles(listing.Photos)

	hashable := listingWithHashablePhotos(listing)
	if p.Deduplicator != nil && len(hashable.Photos) > 0 {
		duplicate, err := p.Deduplicator.CheckDuplicate(ctx, hashable)
		if err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			event := pipelineEventWithError(started, source, listing, "dedup_checked", record.Status, "", err)
			event.HashablePhotoCount = len(hashable.Photos)
			p.logPipeline(ctx, event)
			return p.finish(ctx, started, record)
		}
		if duplicate != nil {
			record.Duplicate = duplicate
			record.Status = "duplicate"
			event := pipelineEvent(started, source, listing, "dedup_checked", record.Status, "duplicate listing image fingerprint")
			event.HashablePhotoCount = len(hashable.Photos)
			p.logPipeline(ctx, event)
			return p.finish(ctx, started, record)
		}
		event := pipelineEvent(started, source, listing, "dedup_checked", "passed", "")
		event.HashablePhotoCount = len(hashable.Photos)
		p.logPipeline(ctx, event)
	}

	if p.Analyzer == nil {
		record.Status = "failed"
		record.FailureReason = "AI analyzer is not configured"
		p.logPipeline(ctx, pipelineEvent(started, source, listing, "vision_called", record.Status, record.FailureReason))
		return p.finish(ctx, started, record)
	}

	insights, usage, err := p.Analyzer.AnalyzeWithUsage(ctx, listing)
	if err != nil {
		record.Status = "failed"
		record.FailureReason = err.Error()
		record.AIUsage = &usage
		p.logPipeline(ctx, pipelineEventWithError(started, source, listing, "vision_called", record.Status, "", err))
		return p.finish(ctx, started, record)
	}
	p.logPipeline(ctx, pipelineEvent(started, source, listing, "vision_called", "passed", ""))
	record.Insights = &insights
	record.AIUsage = &usage
	listing = scoringListingWithInsights(listing, insights)
	record.Listing = listing
	score := scoring.ScoreWithInsights(listing, p.Prefs, insights)
	record.Score = &score
	if score.PassedHardFilters || p.FilterDisabled {
		record.Status = "passed"
	} else {
		record.Status = "rejected_ai"
	}
	p.logPipeline(ctx, pipelineEvent(started, source, listing, "scored", record.Status, strings.Join(score.RejectionReasons, "; ")))

	if p.Deduplicator != nil && len(hashable.Photos) > 0 {
		if err := p.Deduplicator.SaveFingerprint(ctx, listing.ID, hashable.Photos); err != nil {
			record.Status = "failed"
			record.FailureReason = err.Error()
			p.logPipeline(ctx, pipelineEventWithError(started, source, listing, "dedup_saved", record.Status, "", err))
			return p.finish(ctx, started, record)
		}
		event := pipelineEvent(started, source, listing, "dedup_saved", "saved", "")
		event.HashablePhotoCount = len(hashable.Photos)
		p.logPipeline(ctx, event)
	}
	return p.finish(ctx, started, record)
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
	text := strings.ToLower(strings.TrimSpace(message.Text))
	if text == "" {
		return len(message.Photos) > 0
	}

	// 1. Service messages / advertisements / admin notices
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

	// 2. Rental signals (very strong indicators of rental listings)
	rentalSignals := []string{
		"аренд", // аренда, аренду, арендовать, под аренду, etc.
		"сдам",
		"сдаю",
		"сдаёт",
		"сдается",
		"сдаётся",
		"снять",
		"посуточно",
		"помесячно",
		"длительно",
		"rent",
		"lease",
		"ქირავდება",
		"გაქირავება",
	}

	// 3. Housing type signals
	housingSignals := []string{
		"квартир",    // квартира, квартиру, квартиры, квартир
		"апартамент", // апартаменты, апартамент
		"студи",      // студия, студию, студии
		"комнат",     // комната, комнат, комнаты
		"спальн",     // спальня, спальни, спален
		"новострой",  // новостройка, новострой
		"flat",
		"apartment",
		"studio",
		"bedroom",
		"house",
		"ბინა",
		"სტუდიო",
		"ოთახი",
	}

	// 4. Layout pattern (e.g., 1+1, 2+1, 1+0, etc.)
	layoutRegex := regexp.MustCompile(`\b\d\s*\+\s*\d\b`)
	hasLayout := layoutRegex.MatchString(text)

	// 5. Sale signals
	saleSignals := []string{
		"продам",
		"продажа",
		"продается",
		"продаётся",
		"sale",
	}

	hasServiceSignal := containsAnySubstring(text, serviceSignals...)
	hasHousing := containsAnySubstring(text, housingSignals...) || hasLayout
	hasRental := containsAnySubstring(text, rentalSignals...)
	hasSale := containsAnySubstring(text, saleSignals...)

	// A valid listing should have:
	// - A rental signal, OR
	// - A housing signal combined with a sale signal, OR
	// - A housing signal combined with a price/area/photo
	hasValidPropertySignal := false
	if hasRental {
		hasValidPropertySignal = true
	} else if hasHousing && hasSale {
		hasValidPropertySignal = true
	} else if hasHousing && (listing.Price > 0 || listing.AreaM2 > 0 || len(message.Photos) > 0) {
		hasValidPropertySignal = true
	}

	if hasServiceSignal && !hasValidPropertySignal {
		return false
	}
	if strings.Contains(text, "групп") && strings.Contains(text, "недвижимост") && !hasValidPropertySignal {
		return false
	}

	return hasValidPropertySignal
}

func containsAnySubstring(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func (p Processor) geocodeListing(ctx context.Context, source domain.ListingSourceRef, listing domain.ApartmentListing) domain.ApartmentListing {
	if p.Geocoder == nil || listing.Latitude != 0 || listing.Longitude != 0 {
		return listing
	}
	for _, candidate := range geocodeListingQueries(listing, p.searchCity()) {
		loc, err := p.Geocoder.Resolve(ctx, candidate.Query, candidate.MaxDistanceMeters)
		p.logGeocodeAttempt(ctx, GeocodeLogEvent{
			Time:              time.Now().UTC(),
			Source:            source,
			ListingID:         listing.ID,
			ListingTitle:      listing.Title,
			Query:             p.geocodeRequestQuery(candidate.Query),
			MaxDistanceMeters: candidate.MaxDistanceMeters,
			ResultName:        loc.Name,
			ResultLatitude:    loc.Latitude,
			ResultLongitude:   loc.Longitude,
			Success:           err == nil && loc.Latitude != 0 && loc.Longitude != 0,
			Error:             errorString(err),
		})
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

func (p Processor) searchCity() cities.City {
	if p.City.Key == "" {
		return cities.Batumi
	}
	return p.City
}

func ProfileScopedListingID(profileID, listingID string) string {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" || strings.HasPrefix(listingID, profileID+"/") {
		return listingID
	}
	return profileID + "/" + listingID
}

func (p Processor) geocodeRequestQuery(query string) string {
	formatter, ok := p.Geocoder.(geocodeRequestQueryFormatter)
	if !ok {
		return query
	}
	formatted := strings.TrimSpace(formatter.RequestQuery(query))
	if formatted == "" {
		return query
	}
	return formatted
}

func (p Processor) logGeocodeAttempt(ctx context.Context, event GeocodeLogEvent) {
	if p.GeocodeLog == nil {
		return
	}
	_ = p.GeocodeLog.LogGeocodeAttempt(ctx, event)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func pipelineEvent(started time.Time, source domain.ListingSourceRef, listing domain.ApartmentListing, stage, status, reason string) PipelineLogEvent {
	return PipelineLogEvent{
		Time:         time.Now().UTC(),
		Source:       source,
		ListingID:    listing.ID,
		ListingTitle: listing.Title,
		Stage:        stage,
		Status:       status,
		Reason:       strings.TrimSpace(reason),
		Price:        listing.Price,
		AreaM2:       listing.AreaM2,
		Latitude:     listing.Latitude,
		Longitude:    listing.Longitude,
		PhotoCount:   len(listing.Photos),
		DurationMS:   time.Since(started).Milliseconds(),
	}
}

func pipelineEventWithError(started time.Time, source domain.ListingSourceRef, listing domain.ApartmentListing, stage, status, reason string, err error) PipelineLogEvent {
	event := pipelineEvent(started, source, listing, stage, status, reason)
	event.Error = errorString(err)
	return event
}

func (p Processor) logPipeline(ctx context.Context, event PipelineLogEvent) {
	if p.PipelineLog == nil {
		return
	}
	_ = p.PipelineLog.LogPipelineEvent(ctx, event)
}

var addressLinePattern = regexp.MustCompile(`(?i)(^|[\s,])(?:ул\.?|улиц[аеуы]?|street|st\.?|avenue|ave\.?|проспект|пр-т|переулок|пер\.?|road|rd\.?|lane|ln\.?)\s+`)
var addressLabelPattern = regexp.MustCompile(`(?i)^\s*(?:address|адрес)\s*:\s*`)
var streetCasePattern = regexp.MustCompile(`(?i)^улиц[еуы]`)

type geocodeListingQuery struct {
	Query             string
	MaxDistanceMeters int
}

func geocodeListingQueries(listing domain.ApartmentListing, city cities.City) []geocodeListingQuery {
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
		add(city.QueryWithRegion(query), city.ListingAddressSearchRadiusM)
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

func (p Processor) finish(ctx context.Context, started time.Time, record domain.ListingEvaluationRecord) (Result, error) {
	record.Listing = listingWithoutLocalPhotoFiles(record.Listing)
	record.Report = BuildReport(record, p.Prefs)
	err := p.save(ctx, record)
	if err != nil {
		p.logPipeline(ctx, pipelineEventWithError(started, record.Source, record.Listing, "saved", "failed", "", err))
	} else {
		p.logPipeline(ctx, pipelineEvent(started, record.Source, record.Listing, "saved", record.Status, ""))
	}
	return Result{Record: record}, err
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
