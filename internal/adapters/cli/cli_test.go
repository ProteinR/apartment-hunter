package cli

import (
	"context"
	"errors"
	"testing"

	"apartment-hunter/internal/adapters/googlemaps"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"
)

func TestShouldSendTelegramNotificationHonorsDebugMode(t *testing.T) {
	enabled := true
	disabled := false

	if !shouldSendTelegramNotification(profiles.Profile{DebugMode: &enabled}, domain.ListingEvaluationRecord{Status: "rejected_basic"}) {
		t.Fatal("expected rejected listing to be sent when debug mode is enabled")
	}
	if shouldSendTelegramNotification(profiles.Profile{DebugMode: &disabled}, domain.ListingEvaluationRecord{Status: "rejected_basic"}) {
		t.Fatal("expected rejected listing to be skipped when debug mode is disabled")
	}
	if !shouldSendTelegramNotification(profiles.Profile{DebugMode: &disabled}, domain.ListingEvaluationRecord{Status: "passed"}) {
		t.Fatal("expected passed listing to be sent when debug mode is disabled")
	}
}

func TestShouldSendListingSearchAreaMap(t *testing.T) {
	record := domain.ListingEvaluationRecord{
		Status: "rejected_location",
		Listing: domain.ApartmentListing{
			Latitude:  41.6466,
			Longitude: 41.6266,
		},
	}
	if !shouldSendListingSearchAreaMap(record) {
		t.Fatal("expected location rejection with coordinates to be map eligible")
	}
	record.Status = "passed"
	if !shouldSendListingSearchAreaMap(record) {
		t.Fatal("expected passed listing with coordinates to be map eligible")
	}
	record.Status = "rejected_basic"
	if shouldSendListingSearchAreaMap(record) {
		t.Fatal("expected basic rejection to skip map")
	}
	record.Status = "rejected_location"
	record.Listing = domain.ApartmentListing{}
	if shouldSendListingSearchAreaMap(record) {
		t.Fatal("expected listing without coordinates to skip map")
	}
}

func TestListingSearchAreaMapURL(t *testing.T) {
	store := &fakeSearchAreaStore{geoJSON: `{"type":"MultiPolygon","coordinates":[[[[41.620000,41.640000],[41.630000,41.640000],[41.630000,41.650000],[41.620000,41.650000],[41.620000,41.640000]]]]}`}
	record := domain.ListingEvaluationRecord{
		Status: "rejected_location",
		Source: domain.ListingSourceRef{ProfileID: "telegram_42"},
		Listing: domain.ApartmentListing{
			Latitude:  41.6466,
			Longitude: 41.6266,
		},
	}

	mapURL, ok, err := listingSearchAreaMapURL(context.Background(), store, googlemaps.StaticMapRenderer{APIKey: "test-key"}, record)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || mapURL == "" {
		t.Fatalf("expected map URL, ok=%t url=%q", ok, mapURL)
	}
	if store.profileID != "telegram_42" {
		t.Fatalf("unexpected profile id %q", store.profileID)
	}
}

func TestListingSearchAreaMapURLReturnsStoreErrors(t *testing.T) {
	store := &fakeSearchAreaStore{err: errors.New("database unavailable")}
	record := domain.ListingEvaluationRecord{
		Status: "passed",
		Source: domain.ListingSourceRef{ProfileID: "telegram_42"},
		Listing: domain.ApartmentListing{
			Latitude:  41.6466,
			Longitude: 41.6266,
		},
	}

	_, ok, err := listingSearchAreaMapURL(context.Background(), store, googlemaps.StaticMapRenderer{APIKey: "test-key"}, record)
	if err == nil {
		t.Fatal("expected store error")
	}
	if ok {
		t.Fatal("expected no map when store fails")
	}
}

func TestResolvedChannelSourceKeepsUsernameAndUsesTelegramID(t *testing.T) {
	source := resolvedChannelSource(
		sources.ChannelRef{ID: "fakemassagechat", Username: "fakemassagechat"},
		[]sources.ChannelMessage{{
			ChannelID:       "2967368875",
			ChannelName:     "TRUMP CRYPTO PREDICTION CHANNEL DEV",
			ChannelUsername: "fakemassagechat",
			MessageID:       3750,
		}},
	)

	if source.ID != "2967368875" {
		t.Fatalf("expected numeric channel id, got %q", source.ID)
	}
	if source.Username != "fakemassagechat" {
		t.Fatalf("expected username to be preserved, got %q", source.Username)
	}
	if source.Title != "TRUMP CRYPTO PREDICTION CHANNEL DEV" {
		t.Fatalf("expected title to be updated, got %q", source.Title)
	}
}

type fakeSearchAreaStore struct {
	geoJSON   string
	ok        bool
	err       error
	profileID string
}

func (s *fakeSearchAreaStore) LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error) {
	s.profileID = profileID
	if s.err != nil {
		return "", false, s.err
	}
	ok := s.ok || s.geoJSON != ""
	return s.geoJSON, ok, nil
}
