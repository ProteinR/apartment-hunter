package geocodingmock

import (
	"strings"

	"apartment-hunter/internal/domain"
)

var knownLocations = map[string]domain.LocationPreference{
	"6 may park": {
		Name:      "6 May Park",
		Latitude:  41.6466,
		Longitude: 41.6266,
	},
	"new boulevard": {
		Name:      "New Boulevard",
		Latitude:  41.6339,
		Longitude: 41.6077,
	},
}

func Resolve(name string, maxDistanceMeters int) domain.LocationPreference {
	key := strings.ToLower(strings.TrimSpace(name))
	if loc, ok := knownLocations[key]; ok {
		loc.MaxDistanceMeters = maxDistanceMeters
		return loc
	}

	return domain.LocationPreference{
		Name:              strings.TrimSpace(name),
		MaxDistanceMeters: maxDistanceMeters,
	}
}
