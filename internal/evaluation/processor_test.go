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

func (g *fakeGeocoder) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	g.calls++
	return domain.LocationPreference{Name: "Europe Square, Batumi", Latitude: 41.6511, Longitude: 41.6367}, nil
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
