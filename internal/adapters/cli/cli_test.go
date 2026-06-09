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
	if shouldSendTelegramNotification(profiles.Profile{DebugMode: &enabled}, domain.ListingEvaluationRecord{Status: "rejected_non_listing"}) {
		t.Fatal("expected non-listing rejection to be skipped when debug mode is enabled")
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

type fakeAlbumTextResolver struct {
	text   string
	err    error
	called bool
	gotID  int64
}

func (r *fakeAlbumTextResolver) FetchAlbumText(ctx context.Context, channel sources.ChannelRef, groupedID int64, anchorMessageID int) (string, error) {
	r.called = true
	r.gotID = groupedID
	return r.text, r.err
}

func TestEnrichGroupedAlbumTextFillsMissingCaption(t *testing.T) {
	resolver := &fakeAlbumTextResolver{text: "#Сдам квартиру 2+1\nЦена 1450$"}
	message := sources.ChannelMessage{
		ChannelID:       "1625798850",
		ChannelUsername: "domikibatumi",
		MessageID:       954706,
		GroupedID:       14248112711694490,
		Photos:          []sources.ChannelPhoto{{ID: "photo-1"}},
	}

	out := enrichGroupedAlbumText(context.Background(), resolver, message, nil)

	if !resolver.called {
		t.Fatal("expected resolver to be called for text-less album fragment")
	}
	if resolver.gotID != 14248112711694490 {
		t.Fatalf("unexpected grouped id passed to resolver: %d", resolver.gotID)
	}
	if out.Text != "#Сдам квартиру 2+1\nЦена 1450$" {
		t.Fatalf("expected album text to be merged in, got %q", out.Text)
	}
}

func TestEnrichGroupedAlbumTextSkipsNonGroupedAndCaptioned(t *testing.T) {
	resolver := &fakeAlbumTextResolver{text: "should not be used"}

	nonGrouped := sources.ChannelMessage{MessageID: 1, Text: ""}
	if out := enrichGroupedAlbumText(context.Background(), resolver, nonGrouped, nil); out.Text != "" {
		t.Fatalf("expected non-grouped message to be left untouched, got %q", out.Text)
	}

	captioned := sources.ChannelMessage{MessageID: 2, GroupedID: 99, Text: "Цена 1000$"}
	if out := enrichGroupedAlbumText(context.Background(), resolver, captioned, nil); out.Text != "Цена 1000$" {
		t.Fatalf("expected captioned album message to keep its text, got %q", out.Text)
	}
	if resolver.called {
		t.Fatal("expected resolver not to be called when text is present or message is not grouped")
	}
}

func TestEnrichGroupedAlbumTextToleratesResolverError(t *testing.T) {
	resolver := &fakeAlbumTextResolver{err: errors.New("telegram unavailable")}
	message := sources.ChannelMessage{MessageID: 3, GroupedID: 99, Photos: []sources.ChannelPhoto{{ID: "p"}}}

	out := enrichGroupedAlbumText(context.Background(), resolver, message, nil)
	if out.Text != "" {
		t.Fatalf("expected message to be returned unchanged on resolver error, got %q", out.Text)
	}
}
