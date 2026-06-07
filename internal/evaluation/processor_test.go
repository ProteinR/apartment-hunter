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

type fakeGeocoder struct {
	calls int
}

type fakeLocationMatcher struct {
	calls   int
	matches bool
	reason  string
}

func (g *fakeGeocoder) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	g.calls++
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
