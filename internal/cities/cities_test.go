package cities

import "testing"

func TestQueryWithRegionAppendsOwnRegionUnlessOwnCityIsMentioned(t *testing.T) {
	cases := []struct {
		city  City
		query string
		want  string
	}{
		{Batumi, "улица Грибоедова,21", "улица Грибоедова,21, Batumi, Georgia"},
		{Batumi, "Tbilisi Square 3", "Tbilisi Square 3, Batumi, Georgia"},
		{Batumi, "Rustaveli 10, Batumi", "Rustaveli 10, Batumi"},
		{Tbilisi, "Vake Park", "Vake Park, Tbilisi, Georgia"},
		{Tbilisi, "ул. Батуми 5", "ул. Батуми 5, Tbilisi, Georgia"},
		{Tbilisi, "Ваке, Тбилиси", "Ваке, Тбилиси"},
	}
	for _, tc := range cases {
		if got := tc.city.QueryWithRegion(tc.query); got != tc.want {
			t.Fatalf("%s %q: got %q, want %q", tc.city.Key, tc.query, got, tc.want)
		}
	}
}

func TestLastMentionedPrefersTheLatestCity(t *testing.T) {
	city, ok := LastMentioned("Переезжаю из Батуми в Тбилиси, ищу квартиру")
	if !ok || city.Key != Tbilisi.Key {
		t.Fatalf("expected Tbilisi, got %+v %t", city, ok)
	}
	if _, ok := LastMentioned("Ищу квартиру около парка"); ok {
		t.Fatal("expected no city")
	}
}

func TestFindAcceptsKeysAliasesAndRegions(t *testing.T) {
	for _, value := range []string{"tbilisi", "Тбилиси", "Tbilisi, Georgia", " TBILISI "} {
		if city, ok := Find(value); !ok || city.Key != Tbilisi.Key {
			t.Fatalf("%q: expected Tbilisi, got %+v", value, city)
		}
	}
	if _, ok := Find("Kutaisi"); ok {
		t.Fatal("expected unsupported city")
	}
}
