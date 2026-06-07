package evaluation

import (
	"context"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/sources"
)

type countingPhotoLoader struct {
	calls int
}

type fakeAnalyzer struct {
	insights domain.ListingInsights
}

type fakeGeocoder struct {
	calls   int
	queries []string
}

type fakeLocationMatcher struct {
	calls   int
	matches bool
	reason  string
}

func (g *fakeGeocoder) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	g.calls++
	g.queries = append(g.queries, query)
	return domain.LocationPreference{Name: "Europe Square, Batumi", Latitude: 41.6511, Longitude: 41.6367}, nil
}

func (m *fakeLocationMatcher) MatchesListingLocation(ctx context.Context, profileID string, listing domain.ApartmentListing) (bool, string, error) {
	m.calls++
	return m.matches, m.reason, nil
}

func (l *countingPhotoLoader) LoadPhotos(ctx context.Context, message sources.ChannelMessage, listing domain.ApartmentListing) (domain.ApartmentListing, error) {
	l.calls++
	return listing, nil
}

func (a fakeAnalyzer) AnalyzeWithUsage(ctx context.Context, listing domain.ApartmentListing) (domain.ListingInsights, domain.AIUsage, error) {
	return a.insights, domain.AIUsage{}, nil
}

func TestProcessMessageGeocodesListingAfterBasicFilters(t *testing.T) {
	geocoder := &fakeGeocoder{}
	processor := Processor{
		Prefs:    domain.UserPreferences{MaxBudget: 800},
		Geocoder: geocoder,
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		Text: "Flat near Europe Square\n650 USD\n50 m2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if geocoder.calls != 1 {
		t.Fatalf("expected geocoder to be called once, got %d", geocoder.calls)
	}
	if result.Record.Listing.Latitude != 41.6511 || result.Record.Listing.Longitude != 41.6367 {
		t.Fatalf("unexpected coordinates %.6f %.6f", result.Record.Listing.Latitude, result.Record.Listing.Longitude)
	}
}

func TestProcessMessageGeocodesExplicitAddressBeforeFullListingText(t *testing.T) {
	geocoder := &fakeGeocoder{}
	processor := Processor{
		Prefs:    domain.UserPreferences{MaxBudget: 800},
		Geocoder: geocoder,
	}

	_, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		Text: "#сдам #2спальни #батуми\n" +
			"3х комнатная квартира рядом с Парком 6 Мая\n" +
			"📍ул. Горгиладзе, 45\n" +
			"✔️75 м2\n" +
			"💸 650$ на год",
	})
	if err != nil {
		t.Fatal(err)
	}
	if geocoder.calls != 1 {
		t.Fatalf("expected geocoder to stop after first successful address query, got %d calls", geocoder.calls)
	}
	if len(geocoder.queries) != 1 || geocoder.queries[0] != "ул. Горгиладзе, 45" {
		t.Fatalf("expected explicit address query first, got %+v", geocoder.queries)
	}
}

func TestProcessMessageSkipsPhotoLoadingWhenBasicFiltersFail(t *testing.T) {
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs:       domain.UserPreferences{MaxBudget: 600},
		PhotoLoader: loader,
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		Text: "Apartment\n700 USD\n50 m2",
		Photos: []sources.ChannelPhoto{{
			ID: "telegram-photo-location:test-token",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Status != "rejected_basic" {
		t.Fatalf("expected rejected_basic, got %s", result.Record.Status)
	}
	if loader.calls != 0 {
		t.Fatalf("expected photo loader not to be called, got %d calls", loader.calls)
	}
}

func TestProcessMessageRejectsServicePostBeforeGeocodingAndAI(t *testing.T) {
	geocoder := &fakeGeocoder{}
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs: domain.UserPreferences{
			MaxBudget:         650,
			MinAreaM2:         50,
			RequireDishwasher: true,
			RequireOven:       true,
			Locations: []domain.LocationPreference{{
				Name:              "Batumi, Georgia",
				Latitude:          41.64610,
				Longitude:         41.64049,
				MaxDistanceMeters: 1000,
			}},
		},
		Geocoder:    geocoder,
		PhotoLoader: loader,
		Analyzer: fakeAnalyzer{insights: domain.ListingInsights{
			InteriorQuality: 1,
			ViewQuality:     1,
		}},
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		ChannelID: "domikibatumi",
		MessageID: 952575,
		Text: "Константин Кулумбаев 👋🏻\n" +
			"Добро пожаловать в группу по недвижимости в Батуми!\n" +
			"📌 Приобретите абонемент для публикации объявлений.\n\n" +
			"🏠 По вопросам оплаты и активации абонемента — пишите администратору в личные сообщения. @pr_manager19\n\n" +
			"✅ Правила и подробности размещения — в закреплённом сообщении.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Status != "rejected_non_listing" {
		t.Fatalf("expected rejected_non_listing, got %s", result.Record.Status)
	}
	if geocoder.calls != 0 {
		t.Fatalf("expected geocoder not to be called, got %d calls", geocoder.calls)
	}
	if loader.calls != 0 {
		t.Fatalf("expected photo loader not to be called, got %d calls", loader.calls)
	}
	if result.Record.AIUsage != nil {
		t.Fatalf("expected AI not to be called, got usage %+v", result.Record.AIUsage)
	}
}

func TestProcessMessageDoesNotBasicRejectUnknownArea(t *testing.T) {
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs:       domain.UserPreferences{MaxBudget: 800, MinAreaM2: 50},
		PhotoLoader: loader,
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		Photos: []sources.ChannelPhoto{{
			ID: "telegram-photo-location:test-token",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Status == "rejected_basic" {
		t.Fatalf("expected unknown area not to be rejected by basic filters, got reasons %v", result.Record.Score.RejectionReasons)
	}
	if result.Record.Status != "failed" || result.Record.FailureReason != "AI analyzer is not configured" {
		t.Fatalf("expected processing to continue past basic filters, got status=%s failure=%q", result.Record.Status, result.Record.FailureReason)
	}
	if loader.calls != 1 {
		t.Fatalf("expected photo loader to be called once, got %d calls", loader.calls)
	}
}

func TestProcessMessageRejectsLocationBeforePhotoLoading(t *testing.T) {
	geocoder := &fakeGeocoder{}
	matcher := &fakeLocationMatcher{matches: false, reason: "listing location is outside preferred search area"}
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs:       domain.UserPreferences{MaxBudget: 800},
		Geocoder:    geocoder,
		Location:    matcher,
		PhotoLoader: loader,
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{ProfileID: "telegram_42"}, sources.ChannelMessage{
		Text: "Flat near Europe Square\n650 USD\n50 m2",
		Photos: []sources.ChannelPhoto{{
			ID: "telegram-photo-location:test-token",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Status != "rejected_location" {
		t.Fatalf("expected rejected_location, got %s", result.Record.Status)
	}
	if geocoder.calls != 1 {
		t.Fatalf("expected geocoder to be called once, got %d", geocoder.calls)
	}
	if matcher.calls != 1 {
		t.Fatalf("expected location matcher to be called once, got %d", matcher.calls)
	}
	if loader.calls != 0 {
		t.Fatalf("expected photo loader not to be called, got %d calls", loader.calls)
	}
}

func TestProcessMessageLoadsPhotosWhenLocationMatches(t *testing.T) {
	matcher := &fakeLocationMatcher{matches: true}
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs:       domain.UserPreferences{MaxBudget: 800},
		Location:    matcher,
		PhotoLoader: loader,
	}

	_, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{ProfileID: "telegram_42"}, sources.ChannelMessage{
		Text: "Flat\n650 USD\n50 m2\n41.6511,41.6367",
		Photos: []sources.ChannelPhoto{{
			ID: "telegram-photo-location:test-token",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if matcher.calls != 1 {
		t.Fatalf("expected location matcher to be called once, got %d", matcher.calls)
	}
	if loader.calls != 1 {
		t.Fatalf("expected photo loader to be called once, got %d calls", loader.calls)
	}
}

func TestProcessMessageDoesNotRejectMissingAppliancesBeforePhotoAnalysis(t *testing.T) {
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs: domain.UserPreferences{
			MaxBudget:         800,
			MinAreaM2:         50,
			RequireDishwasher: true,
			RequireOven:       true,
		},
		PhotoLoader: loader,
		Analyzer: fakeAnalyzer{insights: domain.ListingInsights{
			HasDishwasher:   true,
			HasOven:         true,
			InteriorQuality: 0.7,
			ViewQuality:     0.6,
		}},
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		Text: "Apartment\n650 USD\n55 m2",
		Photos: []sources.ChannelPhoto{{
			ID: "telegram-photo-location:test-token",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if loader.calls != 1 {
		t.Fatalf("expected photo loader to be called once, got %d calls", loader.calls)
	}
	if !result.Record.Listing.HasDishwasher || !result.Record.Listing.HasOven {
		t.Fatalf("expected appliances from insights to be reflected on listing: %+v", result.Record.Listing)
	}
	if result.Record.Status != "passed" {
		t.Fatalf("expected listing to pass after appliance insights, got %s with reasons %v", result.Record.Status, result.Record.Score.RejectionReasons)
	}
}
