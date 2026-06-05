package geocoding

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"apartment-hunter/internal/domain"
)

type Resolver interface {
	Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error)
}

type ResolverFunc func(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error)

func (f ResolverFunc) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	return f(ctx, query, maxDistanceMeters)
}

var (
	coordinatePattern = regexp.MustCompile(`^\s*(-?\d+(?:[.,]\d+)?)\s*[,;\s]\s*(-?\d+(?:[.,]\d+)?)\s*$`)
	embeddedPattern   = regexp.MustCompile(`(?:@|[?&](?:q|ll)=|\b)(-?\d{1,2}[.,]\d+)\s*,\s*(-?\d{1,3}[.,]\d+)`)
)

func ResolveCoordinates(query string, maxDistanceMeters int) (domain.LocationPreference, bool, error) {
	match := coordinatePattern.FindStringSubmatch(strings.TrimSpace(query))
	if len(match) != 3 {
		return domain.LocationPreference{}, false, nil
	}
	lat, err := parseCoordinate(match[1])
	if err != nil {
		return domain.LocationPreference{}, true, err
	}
	lng, err := parseCoordinate(match[2])
	if err != nil {
		return domain.LocationPreference{}, true, err
	}
	if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
		return domain.LocationPreference{}, true, fmt.Errorf("координаты вне допустимого диапазона")
	}
	return domain.LocationPreference{
		Name:              fmt.Sprintf("%.6f, %.6f", lat, lng),
		MaxDistanceMeters: maxDistanceMeters,
		Latitude:          lat,
		Longitude:         lng,
	}, true, nil
}

func FindCoordinates(text string) (float64, float64, bool) {
	for _, match := range embeddedPattern.FindAllStringSubmatch(text, -1) {
		if len(match) != 3 {
			continue
		}
		lat, latErr := parseCoordinate(match[1])
		lng, lngErr := parseCoordinate(match[2])
		if latErr == nil && lngErr == nil && lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180 {
			return lat, lng, true
		}
	}
	return 0, 0, false
}

func parseCoordinate(value string) (float64, error) {
	return strconv.ParseFloat(strings.ReplaceAll(value, ",", "."), 64)
}

type Chain []Resolver

func (c Chain) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	var lastErr error
	for _, resolver := range c {
		loc, err := resolver.Resolve(ctx, query, maxDistanceMeters)
		if err == nil && strings.TrimSpace(loc.Name) != "" {
			return loc, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return domain.LocationPreference{}, lastErr
	}
	return domain.LocationPreference{
		Name:              strings.TrimSpace(query),
		MaxDistanceMeters: maxDistanceMeters,
	}, nil
}

type RequiredChain []Resolver

func (c RequiredChain) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	var lastErr error
	for _, resolver := range c {
		loc, err := resolver.Resolve(ctx, query, maxDistanceMeters)
		if err == nil && strings.TrimSpace(loc.Name) != "" {
			return loc, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if lastErr != nil {
		return domain.LocationPreference{}, lastErr
	}
	return domain.LocationPreference{}, nil
}
