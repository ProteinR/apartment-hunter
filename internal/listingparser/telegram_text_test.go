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

func TestFromChannelMessageParsesPriceIgnoringDeposit(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{
			name: "deposit before price",
			text: "🟢депозит 200$.\nЦена 800$ аренда на год",
			want: 800,
		},
		{
			name: "price before deposit",
			text: "Цена 800$, залог 400$ за последний месяц",
			want: 800,
		},
		{
			name: "only deposit mentions",
			text: "Депозит 300$, без комиссии",
			want: 300, // Fallback to first if all skipped
		},
		{
			name: "rent and deposit on same line",
			text: "Аренда 800$ в месяц, залог 800$",
			want: 800,
		},
		{
			name: "deposit with rent keyword after it",
			text: "депозит 200$, цена 600$",
			want: 600,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listing := FromChannelMessage(sources.ChannelMessage{Text: tt.text})
			if listing.Price != tt.want {
				t.Fatalf("expected price %d, got %d", tt.want, listing.Price)
			}
		})
	}
}

func TestFromChannelMessageParsesPriceWithEmojisAndCurrencyWords(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{
			name: "domikibatumi 1143467 heavy dollar sign after number",
			text: "#сдам квартиру 2+1\nУл👈 Горгиладзе 40\n\n💸цена 1500 💲  на год\nОплата первого и последнего месяца\n\n📍 130 м²\n📍 3 этаж",
			want: 1500,
		},
		{name: "heavy dollar sign glued", text: "Цена 700💲", want: 700},
		{name: "heavy dollar sign with variation selector", text: "Цена 700 💲️", want: 700},
		{name: "heavy dollar sign before number", text: "💲 650 в месяц", want: 650},
		{name: "keyword price before emoji and stray dollar", text: "Цена 900 🔥 $", want: 900},
		{name: "emoji between dollar and number", text: "$ 🔥 900 в месяц", want: 900},
		{name: "money bag before number with dollar", text: "💰1200$", want: 1200},
		{name: "dotted thousands", text: "Цена 1.500$", want: 1500},
		{name: "spaced thousands and dollar", text: "Цена 1 500 $", want: 1500},
		{name: "keyword colon without currency", text: "2+1, 70 м²\nцена:1500", want: 1500},
		{name: "keyword with emoji without currency", text: "Цена 💰 1100 на год", want: 1100},
		{name: "money emoji without keyword or currency", text: "Сдается 1+1\n💵 800\n5 этаж", want: 800},
		{name: "english dash price", text: "Price - 1500$", want: 1500},
		{name: "usd word", text: "Rent 950 usd per month", want: 950},
		{name: "per month suffix", text: "500$/мес", want: 500},
		{name: "lari word", text: "Цена 2000 лари", want: 2000},
		{name: "lari symbol", text: "Цена 2000₾", want: 2000},
		{name: "gel word", text: "Price 1800 GEL", want: 1800},
		{name: "dollar word abbreviation", text: "Сдам 1+1 за 600 дол.", want: 600},
		{name: "dollar word", text: "Сдам 1+1 за 600 долларов", want: 600},
		{name: "deposit without currency before price", text: "Депозит 300$\nЦена: 900", want: 900},
		{name: "deposit without currency after price", text: "💸 Цена 1000 💲\n💲 депозит 500", want: 1000},
		{name: "keyword range prefers dollar-tagged upper bound", text: "Цена 500-600$", want: 600},
		{name: "phone is not price", text: "Сдам 2+1\nЦена 750$\nТел +995 555000102", want: 750},
		{name: "phone only", text: "Сдам 2+1, 60 м², 4 этаж\nТел +995 555000102", want: 0},
		{name: "keyword area is not price", text: "💰 130 м²\n3 этаж", want: 0},
		{name: "keyword small number is not price", text: "💰 2 месяца депозит", want: 0},
		{name: "long word starting with dol is not currency", text: "Сдам на 12 долгих месяцев", want: 0},
		{name: "keyword price followed by floor after comma", text: "Цена 600, 2 этаж", want: 600},
		{name: "keyword price followed by floor", text: "Цена 700 3 этаж", want: 700},
		{name: "keyword price followed by deposit months", text: "Стоимость 500 1 месяц депозит", want: 500},
		{name: "keyword followed by spaced phone", text: "Цена 💰 555 123 456", want: 0},
		{name: "money emoji followed by spaced phone", text: "💵 555 123 456", want: 0},
		{name: "comma thousands", text: "$1,200 monthly", want: 1200},
		{name: "room count before em dash and dollar", text: "2+1 — $650 в месяц", want: 650},
		{name: "floor before dash and dollar", text: "Этаж 5 - $500", want: 500},
		{name: "floor fraction before emoji and dollar", text: "🏢 12/16 🔥 $800", want: 800},
		{name: "room count before heavy dollar and price", text: "Сдается 2+1 💲650", want: 650},
		{name: "number sign before heavy dollar and price", text: "№12 💲700", want: 700},
		{name: "hyphenated phone before heavy dollar", text: "📞 555-12-34-56 💲", want: 0},
		{name: "bare range keeps upper bound", text: "500-600$", want: 600},
		{name: "trailing dollar price before floor", text: "500$ 3 этаж", want: 500},
		{name: "trailing dollar price before dash and floor", text: "500$ — 3 этаж", want: 500},
		{name: "trailing dollar price before room count", text: "1500$ 2+1 Новый бульвар", want: 1500},
		{name: "trailing dollar price before deposit months", text: "650$ 1 месяц депозит", want: 650},
		{name: "trailing dollar price before emoji and minutes", text: "500$ 🔥 5 минут до моря", want: 500},
		{name: "rent price before emoji and minimum months", text: "Аренда 450$ 🔑 2 месяца минимум", want: 450},
		{name: "slash separated summary with trailing dollar price", text: "1+1 / 45 м² / 5 этаж / 450$ 🔥 1 месяц депозит", want: 450},
		{name: "room count then trailing dollar price then floor", text: "Сдам 1+1 500$ 3 этаж", want: 500},
		{name: "lari price with dollar equivalent", text: "Цена 2000 лари (≈750$)", want: 750},
		{name: "gel price with dollar equivalent", text: "2000 GEL (≈750$)", want: 750},
		{name: "lari price with approximate dollar after slash", text: "3000 лари / ~1100$", want: 1100},
		{name: "lari symbol only", text: "Цена 2500₾ в месяц", want: 2500},
		{name: "money emoji distance in meters word", text: "💰 300 метров от моря", want: 0},
		{name: "keyword distance in meters word", text: "Цена 300 метров до моря", want: 0},
		{name: "keyword distance with abbreviated meters", text: "💰 300 м. до моря", want: 0},
		{name: "money emoji price before distance", text: "💵 600 300 метров до моря", want: 600},
		{name: "keyword year before dollar price", text: "Цена 2024 года ремонт 600$", want: 600},
		{name: "keyword year without currency", text: "Цена 2024 г. ремонт", want: 0},
		{name: "floor number before dollar price", text: "Этаж 3 450$", want: 450},
		{name: "area before dollar price", text: "45 м2 500$", want: 500},
		{name: "glued area before dollar price", text: "Площадь 40м2 450$", want: 450},
		{name: "spaced thousands after keyword colon", text: "Цена: 1 500$", want: 1500},
		{name: "floor number before floor word then dollar price", text: "5 этаж 700$", want: 700},
		{name: "summary with floor word then dollar price", text: "2+1, 60 м², 5 этаж 700$", want: 700},
		{name: "floor word then grouped dollar price", text: "5 этаж 1 200$", want: 1200},
		{name: "floor word directly before ungrouped dollar price", text: "Этаж: 700$", want: 700},
		{name: "spaced international phone before heavy dollar price", text: "📞 +995 555 123 456 💲 700", want: 700},
		{name: "glued phone before heavy dollar price", text: "📞 555123456 💲 700", want: 700},
		{name: "grouped phone before heavy dollar price", text: "WhatsApp 555 12 34 56 💲 700", want: 700},
		{name: "grouped phone before dollar price", text: "Тел 555 12 34 56 $700", want: 700},
		{name: "decimal dollar price", text: "Цена 450.50$", want: 450},
		{name: "lari rent with separate dollar parking", text: "Цена: 1800₾\nПарковка 30$", want: 1800},
		{name: "gel rent with dollar utilities on same line", text: "Цена 2000 GEL, комм. 50$", want: 2000},
		{name: "gel rent with dollar cleaning", text: "Price 1500 GEL\nCleaning 20$ per visit", want: 1500},
		{name: "lari symbol with dollar equivalent in parentheses", text: "1800 ₾ ($660)", want: 660},
		{name: "gel price with dollar equivalent after or", text: "2000 GEL или 750$", want: 750},
		{name: "keyword percent prepayment before gel price", text: "💰 100% предоплата\nЦена 1500 GEL", want: 1500},
		{name: "keyword area in sq m before gel price", text: "💰 150 sq.m\nЦена 1500 GEL", want: 1500},
		{name: "keyword area in sq m before gel price on same line", text: "Цена 💰 300 sq.m 900 GEL", want: 900},
		{name: "heavy dollar month count before price", text: "💲 2 мес депозит 💲 700", want: 700},
		{name: "keyword rent before dollar parking on next line", text: "Цена 1500\nПарковка 30$", want: 1500},
		{name: "gel price with dollar equivalent on next line", text: "Цена 1500 GEL\nВ долларах 560$", want: 560},
		{name: "lari price with dollar equivalent in parentheses on next line", text: "1500 лари\n(550$)", want: 550},
		{name: "gel price with usd alternative on next line", text: "Цена 1600 GEL\nили 600 USD", want: 600},
		{name: "lari symbol with approximate dollar on next line", text: "Цена 1700₾\n≈ 630$", want: 630},
		{name: "gel price with approximate usd in parentheses on next line", text: "Цена 1700 GEL\n(≈630 USD)", want: 630},
		{name: "money emoji lari price with leading dollar on next line", text: "💵 Цена 1700 ₾\n$ 630", want: 630},
		{name: "lari price with dollar equivalent after blank line", text: "Цена: 1500 ₾\n\n💵 550 $", want: 550},
		{name: "lari price with leading dollar on next line", text: "Цена 3000 лари\n$1100", want: 1100},
		{name: "lari rent with dollar option after dash", text: "Аренда 1200 лари\nМожно в долларах — 450$", want: 450},
		{name: "lari utilities before dollar rent", text: "Коммуналка 100 лари\nЦена 600$", want: 600},
		{name: "lari internet before dollar rent with emojis", text: "💰 Интернет 100 лари\n💵 Цена 600$", want: 600},
		{name: "gel utilities before dollar rent", text: "Цена: 300 GEL коммунальные\nАренда: 650$", want: 650},
		{name: "keyword year then dollar price on next line", text: "Price 2024 renovated\n$600", want: 600},
		{name: "keyword multi-month total then monthly dollar price", text: "Цена 1200 за 2 месяца\n$600 в месяц", want: 600},
		{name: "dollar rent with lari utilities on same line", text: "Цена 600$, коммуналка 100 лари", want: 600},
		{name: "bright apartment description with dollar price", text: "Светлая квартира, 600$", want: 600},
		{name: "rent line mentioning utilities before building fee", text: "💰 Цена: 700$ + коммунальные услуги\n🏢 Обслуживание здания 30 лари", want: 700},
		{name: "rent line with utilities in parentheses before garbage fee", text: "Цена 700$ (+ коммуналка)\nВывоз мусора 5 лари", want: 700},
		{name: "rent line with internet clause before house fee", text: "Цена 700$/мес, интернет включен\nДом: 20 лари в месяц", want: 700},
		{name: "rent excluding utilities before maintenance fee", text: "Цена 700$ без учета коммунальных\nКвартплата 40 лари", want: 700},
		{name: "rent with parking before association fee", text: "Аренда с парковкой 1000$\nОСМД 50 лари", want: 1000},
		{name: "dollar rent plus utilities before lari amount", text: "650$ + коммунальные\n1750 лари", want: 650},
		{name: "monthly dollar rent before daily lari price", text: "Сдается квартира 700$/мес\nПосуточно — цена 120 лари", want: 700},
		{name: "monthly dollar rent before per-day lari price", text: "700$ в месяц\nЦена за сутки 100 лари", want: 700},
		{name: "daily lari price before monthly dollar price", text: "Посуточно 150 лари\nПомесячно 700$", want: 700},
		{name: "only daily price", text: "Посуточно 150 лари", want: 150},
		{name: "lari price with approximate dollar abbreviation", text: "Цена 2400₾ (ок. 700$)", want: 700},
		{name: "lari price with dollar payment option", text: "Цена 2500 лари, при оплате в долларах 700$", want: 700},
		{name: "gel price with dollar prepayment", text: "Цена: 1600 GEL\nПредоплата 600$", want: 1600},
		{name: "leading dollar price before floor word", text: "$ 3 этаж", want: 0},
		{name: "leading dollar per month", text: "$500/мес", want: 500},
		{name: "money emoji distance to sea", text: "💰 100 м от моря", want: 0},
		{name: "keyword followed by grouped phone", text: "Цена 💰 555 12 34 56", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listing := FromChannelMessage(sources.ChannelMessage{Text: tt.text})
			if listing.Price != tt.want {
				t.Fatalf("expected price %d, got %d", tt.want, listing.Price)
			}
		})
	}
}
