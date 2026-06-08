package telegram

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"apartment-hunter/internal/domain"
)

const batumiGeoJSONEditorURL = "https://geojson.io/#map=13/41.6461/41.6405"

type geoJSONGeometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates,omitempty"`
}

type geoJSONFeature struct {
	Type     string          `json:"type"`
	Geometry json.RawMessage `json:"geometry"`
}

type geoJSONFeatureCollection struct {
	Type     string           `json:"type"`
	Features []geoJSONFeature `json:"features"`
}

func looksLikeSearchAreaGeoJSON(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "{") && strings.Contains(text, `"type"`)
}

func searchAreaFromGeoJSON(text string) (*domain.GeoSearchArea, error) {
	polygons, err := geoJSONMultiPolygonCoordinates([]byte(strings.TrimSpace(text)))
	if err != nil {
		return nil, err
	}
	if len(polygons) == 0 {
		return nil, fmt.Errorf("GeoJSON не содержит Polygon или MultiPolygon")
	}
	normalized, err := json.Marshal(map[string]any{
		"type":        "MultiPolygon",
		"coordinates": polygons,
	})
	if err != nil {
		return nil, err
	}
	return &domain.GeoSearchArea{
		WKT:     multiPolygonCoordinatesWKT(polygons),
		GeoJSON: string(normalized),
		SRID:    4326,
	}, nil
}

func geoJSONMultiPolygonCoordinates(data []byte) ([][][][]float64, error) {
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return nil, fmt.Errorf("не могу прочитать GeoJSON: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(typed.Type)) {
	case "featurecollection":
		var collection geoJSONFeatureCollection
		if err := json.Unmarshal(data, &collection); err != nil {
			return nil, fmt.Errorf("не могу прочитать FeatureCollection: %w", err)
		}
		var polygons [][][][]float64
		for _, feature := range collection.Features {
			if len(feature.Geometry) == 0 || string(feature.Geometry) == "null" {
				continue
			}
			featurePolygons, err := geoJSONMultiPolygonCoordinates(feature.Geometry)
			if err != nil {
				return nil, err
			}
			polygons = append(polygons, featurePolygons...)
		}
		return validateMultiPolygonCoordinates(polygons)
	case "feature":
		var feature geoJSONFeature
		if err := json.Unmarshal(data, &feature); err != nil {
			return nil, fmt.Errorf("не могу прочитать Feature: %w", err)
		}
		if len(feature.Geometry) == 0 || string(feature.Geometry) == "null" {
			return nil, fmt.Errorf("Feature не содержит geometry")
		}
		return geoJSONMultiPolygonCoordinates(feature.Geometry)
	case "polygon":
		var geometry geoJSONGeometry
		if err := json.Unmarshal(data, &geometry); err != nil {
			return nil, fmt.Errorf("не могу прочитать Polygon: %w", err)
		}
		var polygon [][][]float64
		if err := json.Unmarshal(geometry.Coordinates, &polygon); err != nil {
			return nil, fmt.Errorf("не могу прочитать координаты Polygon: %w", err)
		}
		return validateMultiPolygonCoordinates([][][][]float64{polygon})
	case "multipolygon":
		var geometry geoJSONGeometry
		if err := json.Unmarshal(data, &geometry); err != nil {
			return nil, fmt.Errorf("не могу прочитать MultiPolygon: %w", err)
		}
		var polygons [][][][]float64
		if err := json.Unmarshal(geometry.Coordinates, &polygons); err != nil {
			return nil, fmt.Errorf("не могу прочитать координаты MultiPolygon: %w", err)
		}
		return validateMultiPolygonCoordinates(polygons)
	default:
		return nil, fmt.Errorf("поддерживаются только GeoJSON Polygon, MultiPolygon, Feature и FeatureCollection")
	}
}

func validateMultiPolygonCoordinates(polygons [][][][]float64) ([][][][]float64, error) {
	for i := range polygons {
		if len(polygons[i]) == 0 {
			return nil, fmt.Errorf("polygon %d не содержит колец", i+1)
		}
		for j := range polygons[i] {
			ring := polygons[i][j]
			if len(ring) < 4 {
				return nil, fmt.Errorf("polygon %d ring %d содержит меньше 4 точек", i+1, j+1)
			}
			for k, coord := range ring {
				if len(coord) < 2 {
					return nil, fmt.Errorf("polygon %d ring %d point %d содержит меньше 2 координат", i+1, j+1, k+1)
				}
				lng, lat := coord[0], coord[1]
				if math.IsNaN(lng) || math.IsInf(lng, 0) || math.IsNaN(lat) || math.IsInf(lat, 0) {
					return nil, fmt.Errorf("polygon %d ring %d point %d содержит нечисловую координату", i+1, j+1, k+1)
				}
				if lng < -180 || lng > 180 || lat < -90 || lat > 90 {
					return nil, fmt.Errorf("polygon %d ring %d point %d вне диапазона longitude/latitude", i+1, j+1, k+1)
				}
				polygons[i][j][k] = []float64{lng, lat}
			}
			if !sameCoordinate(ring[0], ring[len(ring)-1]) {
				polygons[i][j] = append(polygons[i][j], []float64{ring[0][0], ring[0][1]})
			}
		}
	}
	return polygons, nil
}

func sameCoordinate(a, b []float64) bool {
	return len(a) >= 2 && len(b) >= 2 && a[0] == b[0] && a[1] == b[1]
}

func multiPolygonCoordinatesWKT(polygons [][][][]float64) string {
	var b strings.Builder
	b.WriteString("MULTIPOLYGON(")
	for i, polygon := range polygons {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(")
		for j, ring := range polygon {
			if j > 0 {
				b.WriteString(",")
			}
			b.WriteString("(")
			for k, coord := range ring {
				if k > 0 {
					b.WriteString(",")
				}
				fmt.Fprintf(&b, "%.8f %.8f", coord[0], coord[1])
			}
			b.WriteString(")")
		}
		b.WriteString(")")
	}
	b.WriteString(")")
	return b.String()
}
