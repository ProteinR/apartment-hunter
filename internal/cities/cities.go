package cities

import (
	"fmt"
	"strings"
)

type City struct {
	Key                         string
	DisplayName                 string
	GeocodingRegion             string
	Latitude                    float64
	Longitude                   float64
	ListingAddressSearchRadiusM int
	aliases                     []string
}

var Batumi = City{
	Key:                         "batumi",
	DisplayName:                 "Батуми",
	GeocodingRegion:             "Batumi, Georgia",
	Latitude:                    41.64610,
	Longitude:                   41.64049,
	ListingAddressSearchRadiusM: 15000,
	aliases:                     []string{"batumi", "батуми", "ბათუმი"},
}

var Tbilisi = City{
	Key:                         "tbilisi",
	DisplayName:                 "Тбилиси",
	GeocodingRegion:             "Tbilisi, Georgia",
	Latitude:                    41.71510,
	Longitude:                   44.82710,
	ListingAddressSearchRadiusM: 25000,
	aliases:                     []string{"tbilisi", "тбилиси", "თბილისი"},
}

var Supported = []City{Batumi, Tbilisi}

func Find(value string) (City, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return City{}, false
	}
	for _, city := range Supported {
		if value == city.Key || strings.EqualFold(value, city.GeocodingRegion) {
			return city, true
		}
		for _, alias := range city.aliases {
			if value == alias {
				return city, true
			}
		}
	}
	return City{}, false
}

func LastMentioned(text string) (City, bool) {
	text = strings.ToLower(text)
	found := City{}
	foundAt := -1
	for _, city := range Supported {
		for _, alias := range city.aliases {
			if index := strings.LastIndex(text, alias); index > foundAt {
				found = city
				foundAt = index
			}
		}
	}
	return found, foundAt >= 0
}

func (c City) mentionedIn(text string) bool {
	text = strings.ToLower(text)
	for _, alias := range c.aliases {
		if strings.Contains(text, alias) {
			return true
		}
	}
	return false
}

func ForKey(key string, fallback City) City {
	if city, ok := Find(key); ok {
		return city
	}
	return fallback
}

func (c City) IsBatumi() bool {
	return c.Key == Batumi.Key
}

func (c City) QueryWithRegion(query string) string {
	query = strings.TrimSpace(query)
	if query == "" || c.GeocodingRegion == "" {
		return query
	}
	if c.mentionedIn(query) {
		return query
	}
	return query + ", " + c.GeocodingRegion
}

func (c City) GeoJSONEditorURL() string {
	return fmt.Sprintf("https://geojson.io/#map=13/%.4f/%.4f", c.Latitude, c.Longitude)
}

func SupportedDisplayNames() string {
	names := make([]string, 0, len(Supported))
	for _, city := range Supported {
		names = append(names, city.DisplayName)
	}
	return strings.Join(names, ", ")
}
