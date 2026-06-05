package geocodingmock

import (
	"context"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
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

type Resolver struct{}
type KnownResolver struct{}

func New() Resolver {
	return Resolver{}
}

func NewKnown() KnownResolver {
	return KnownResolver{}
}

func (Resolver) Resolve(ctx context.Context, name string, maxDistanceMeters int) (domain.LocationPreference, error) {
	return Resolve(name, maxDistanceMeters), nil
}

func (KnownResolver) Resolve(ctx context.Context, name string, maxDistanceMeters int) (domain.LocationPreference, error) {
	if loc, ok, err := geocoding.ResolveCoordinates(name, maxDistanceMeters); ok {
		return loc, err
	}
	key := strings.ToLower(strings.TrimSpace(name))
	loc, ok := knownLocations[key]
	if !ok {
		return domain.LocationPreference{}, nil
	}
	loc.MaxDistanceMeters = maxDistanceMeters
	return loc, nil
}

func Resolve(name string, maxDistanceMeters int) domain.LocationPreference {
	if loc, ok, err := geocoding.ResolveCoordinates(name, maxDistanceMeters); ok && err == nil {
		return loc
	}
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
