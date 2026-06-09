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

func TestFromChannelMessageParsesCyrillicAreaFormats(t *testing.T) {
	tests := []struct {
		name string
		text string
		want float64
	}{
		{name: "cyrillic m2", text: "Сдаётся квартира\n55 м2\n500 $", want: 55},
		{name: "cyrillic m squared", text: "Площадь: 45 м² / Этаж: 3/9", want: 45},
		{name: "square meters abbreviation", text: "65 кв.м\nцена 800$", want: 65},
		{name: "meters square abbreviation", text: "800$ 1+1 70 м.кв,\nКлдиашвили 14", want: 70},
		{name: "bare square symbol", text: "Цена 450$\n40 ²\nЦентральное отопление", want: 40},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listing := FromChannelMessage(sources.ChannelMessage{Text: tt.text})
			if listing.AreaM2 != tt.want {
				t.Fatalf("expected area %.1f, got %.1f", tt.want, listing.AreaM2)
			}
		})
	}
}

func TestFromChannelMessageParsesDomikiBatumiBasics(t *testing.T) {
	listing := FromChannelMessage(sources.ChannelMessage{
		ChannelID: "domikibatumi",
		MessageID: 952526,
		Text: "сдам квартиру 2+1\n" +
			"🔶12 этаж\n" +
			"🔶50 м²\n" +
			"🔶Духовка\n" +
			"🔶посудомойка\n" +
			"💰Цена 500$",
	})

	if listing.Price != 500 {
		t.Fatalf("expected price 500, got %d", listing.Price)
	}
	if listing.AreaM2 != 50 {
		t.Fatalf("expected area 50, got %.1f", listing.AreaM2)
	}
	if !listing.HasOven {
		t.Fatalf("expected oven to be detected")
	}
	if !listing.HasDishwasher {
		t.Fatalf("expected dishwasher to be detected")
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

func TestFromChannelMessageParsesAlbumCaptionPriceWithTrailingWords(t *testing.T) {
	// Regression for the album-fragment price bug: the caption (recovered from a
	// sibling album message) puts price text inline with trailing words.
	listing := FromChannelMessage(sources.ChannelMessage{
		ChannelID: "1625798850",
		MessageID: 954697,
		Text: "#Сдам квартиру 2+1\n" +
			"Ул. Горгиладзе, 88\n" +
			"🟢80кв.м\n" +
			"🟢духовка\n" +
			"Цена 1450$ аренда на долгий срок",
	})

	if listing.Price != 1450 {
		t.Fatalf("expected price 1450, got %d", listing.Price)
	}
	if listing.AreaM2 != 80 {
		t.Fatalf("expected area 80, got %.1f", listing.AreaM2)
	}
}
