package preferenceparser

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"apartment-hunter/internal/domain"
)

type PostGISGeometryBuilder struct {
	Resolver GeoResolver
	Segments int
}

func (b PostGISGeometryBuilder) BuildSearchGeometry(ctx context.Context, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error) {
	segments := b.Segments
	if segments <= 0 {
		segments = 48
	}

	var polygons [][][]float64
	var unresolved []string
	for i := range prefs {
		loc := prefs[i]
		radius := radiusMeters(loc)
		if radius <= 0 {
			if loc.Type == "area" && strings.TrimSpace(loc.CenterName) != "" {
				unresolved = append(unresolved, "area requires polygon resolution: "+loc.CenterName)
			}
			continue
		}
		lat, lng, err := b.resolveCenter(ctx, loc)
		if err != nil {
			return nil, err
		}
		if lat == 0 && lng == 0 {
			unresolved = append(unresolved, "center unresolved: "+locationQuery(loc))
			continue
		}
		polygons = append(polygons, circlePolygon(lng, lat, radius, segments))
		if loc.BoundaryStreetName != nil && strings.TrimSpace(*loc.BoundaryStreetName) != "" {
			unresolved = append(unresolved, "requires_manual_boundary_resolution: "+*loc.BoundaryStreetName)
		}
		unresolved = append(unresolved, loc.UnresolvedRules...)
	}

	area := &domain.GeoSearchArea{
		SRID:            4326,
		UnresolvedRules: unresolved,
	}
	if len(polygons) == 0 {
		return area, nil
	}
	area.WKT = multiPolygonWKT(polygons)
	geoJSON, err := multiPolygonGeoJSON(polygons)
	if err != nil {
		return nil, err
	}
	area.GeoJSON = geoJSON
	return area, nil
}

func (b PostGISGeometryBuilder) resolveCenter(ctx context.Context, loc domain.LocationPreference) (float64, float64, error) {
	if loc.Latitude != 0 || loc.Longitude != 0 {
		return loc.Latitude, loc.Longitude, nil
	}
	if b.Resolver == nil {
		return 0, 0, nil
	}
	place, err := b.Resolver.ResolvePlace(ctx, locationQuery(loc))
	if err != nil || place == nil {
		return 0, 0, err
	}
	return place.Latitude, place.Longitude, nil
}

func radiusMeters(loc domain.LocationPreference) int {
	if loc.RadiusMeters != nil {
		return *loc.RadiusMeters
	}
	return loc.MaxDistanceMeters
}

func locationQuery(loc domain.LocationPreference) string {
	if strings.TrimSpace(loc.CenterName) != "" {
		return strings.TrimSpace(loc.CenterName)
	}
	return strings.TrimSpace(loc.Name)
}

func circlePolygon(lng, lat float64, radiusMeters, segments int) [][]float64 {
	const earthRadiusMeters = 6378137.0
	coords := make([][]float64, 0, segments+1)
	latRad := lat * math.Pi / 180
	lngRad := lng * math.Pi / 180
	angularDistance := float64(radiusMeters) / earthRadiusMeters
	for i := 0; i <= segments; i++ {
		bearing := 2 * math.Pi * float64(i) / float64(segments)
		pointLat := math.Asin(math.Sin(latRad)*math.Cos(angularDistance) + math.Cos(latRad)*math.Sin(angularDistance)*math.Cos(bearing))
		pointLng := lngRad + math.Atan2(math.Sin(bearing)*math.Sin(angularDistance)*math.Cos(latRad), math.Cos(angularDistance)-math.Sin(latRad)*math.Sin(pointLat))
		coords = append(coords, []float64{pointLng * 180 / math.Pi, pointLat * 180 / math.Pi})
	}
	return coords
}

func multiPolygonWKT(polygons [][][]float64) string {
	var b strings.Builder
	b.WriteString("MULTIPOLYGON(")
	for i, polygon := range polygons {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("((")
		for j, coord := range polygon {
			if j > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%.8f %.8f", coord[0], coord[1])
		}
		b.WriteString("))")
	}
	b.WriteString(")")
	return b.String()
}

func multiPolygonGeoJSON(polygons [][][]float64) (string, error) {
	coordinates := make([][][][]float64, 0, len(polygons))
	for _, polygon := range polygons {
		coordinates = append(coordinates, [][][]float64{polygon})
	}
	data, err := json.Marshal(map[string]any{
		"type":        "MultiPolygon",
		"coordinates": coordinates,
	})
	if err != nil {
		return "", err
	}
	return string(data), nil
}
