package listingparser

import (
	"testing"

	"apartment-hunter/internal/sources"
)

func TestFromChannelMessageParsesListingBasics(t *testing.T) {
	listing := FromChannelMessage(sources.ChannelMessage{
		ChannelID: "batumi-rent",
		MessageID: 12,
		Text:      "Modern flat near boulevard\n45 m2\n$1,200 monthly",
		Photos: []sources.ChannelPhoto{
			{ID: "telegram-photo:1", Caption: "kitchen"},
		},
	})
	if listing.ID != "batumi-rent:12" {
		t.Fatalf("unexpected listing id %q", listing.ID)
	}
	if listing.Title != "Modern flat near boulevard" {
		t.Fatalf("unexpected title %q", listing.Title)
	}
	if listing.AreaM2 != 45 {
		t.Fatalf("expected area 45, got %.1f", listing.AreaM2)
	}
	if listing.Price != 1200 {
		t.Fatalf("expected price 1200, got %d", listing.Price)
	}
	if len(listing.Photos) != 1 || listing.Photos[0].URL != "telegram-photo:1" {
		t.Fatalf("unexpected photos %+v", listing.Photos)
	}
}

func TestFromChannelMessageDoesNotTreatAreaAsPrice(t *testing.T) {
	listing := FromChannelMessage(sources.ChannelMessage{
		ChannelID: "batumi-rent",
		MessageID: 13,
		Text:      "Studio\n45 m2\nOld Boulevard",
	})

	if listing.Price != 0 {
		t.Fatalf("expected no price, got %d", listing.Price)
	}
	if listing.AreaM2 != 45 {
		t.Fatalf("expected area 45, got %.1f", listing.AreaM2)
	}
}

func TestFromChannelMessageParsesCoordinates(t *testing.T) {
	listing := FromChannelMessage(sources.ChannelMessage{
		Text: "Flat near park\nhttps://maps.google.com/?q=41.6466,41.6266\n50 m2\n$600",
	})

	if listing.Latitude != 41.6466 || listing.Longitude != 41.6266 {
		t.Fatalf("unexpected coordinates %.6f %.6f", listing.Latitude, listing.Longitude)
	}
}
