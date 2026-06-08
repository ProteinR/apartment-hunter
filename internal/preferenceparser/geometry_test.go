package preferenceparser

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
)

func TestPostGISGeometryBuilderBuildsRadiusMultiPolygon(t *testing.T) {
	radius := 1000
	boundary := "Pushkin Street, Batumi"
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "6 May Park, Batumi",
			Latitude:  41.6500,
			Longitude: 41.6333,
		}, street: &domain.ResolvedStreet{
			Name:        "Pushkin Street, Batumi",
			GeometryWKT: "LINESTRING(41.6360 41.6580, 41.6435 41.6370)",
		}},
		Segments: 12,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:               "6 May Park zone",
		Type:               "radius_with_boundary",
		CenterName:         "6 May Park, Batumi",
		RadiusMeters:       &radius,
		BoundaryStreetName: &boundary,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if area.SRID != 4326 {
		t.Fatalf("unexpected srid %d", area.SRID)
	}
	if !strings.HasPrefix(area.WKT, "MULTIPOLYGON(((") {
		t.Fatalf("unexpected wkt %q", area.WKT)
	}
	if !strings.Contains(area.GeoJSON, `"MultiPolygon"`) {
		t.Fatalf("unexpected geojson %q", area.GeoJSON)
	}
	if len(area.UnresolvedRules) != 0 {
		t.Fatalf("expected boundary rule to be resolved, got %+v", area.UnresolvedRules)
	}
	for _, point := range firstGeoJSONRing(t, area.GeoJSON) {
		side := lineSide(geomPoint{Lng: 41.6360, Lat: 41.6580}, geomPoint{Lng: 41.6435, Lat: 41.6370}, geomPoint{Lng: point[0], Lat: point[1]})
		centerSide := lineSide(geomPoint{Lng: 41.6360, Lat: 41.6580}, geomPoint{Lng: 41.6435, Lat: 41.6370}, geomPoint{Lng: 41.6333, Lat: 41.6500})
		if side*centerSide < -1e-10 {
			t.Fatalf("point should be clipped to park side of Pushkin Street: %+v", point)
		}
	}
}

func TestPostGISGeometryBuilderAppliesKnownPushkinBoundaryWithoutStreetGeometry(t *testing.T) {
	radius := 1000
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "6 May Park, Batumi",
			Latitude:  41.64800157,
			Longitude: 41.6285588,
		}},
		Segments: 24,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:            "парк 6 мая",
		CenterName:      "парк 6 мая",
		RadiusMeters:    &radius,
		RawText:         "около парка 6 мая в радиусе 1 км, но не дальше улицы Пушкина",
		UnresolvedRules: []string{"но не дальше улицы Пушкина"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(area.UnresolvedRules, "\n"), "Пушкина") {
		t.Fatalf("expected Pushkin rule to be applied, got %+v", area.UnresolvedRules)
	}
	line := knownPushkinBoundaryLine()
	center := geomPoint{Lng: 41.6285588, Lat: 41.64800157}
	centerSide := lineSide(line[0], line[1], center)
	for _, point := range firstGeoJSONRing(t, area.GeoJSON) {
		side := lineSide(line[0], line[1], geomPoint{Lng: point[0], Lat: point[1]})
		if side*centerSide < -1e-10 {
			t.Fatalf("point should be clipped to park side of known Pushkin boundary: %+v", point)
		}
	}
}

func TestPostGISGeometryBuilderAppliesPushkinBoundaryFromBoundaryRule(t *testing.T) {
	radius := 1000
	rule := "not beyond улицы Пушкина"
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "6 May Park, Batumi",
			Latitude:  41.64800157,
			Longitude: 41.6285588,
		}},
		Segments: 24,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:         "парк 6 мая",
		CenterName:   "парк 6 мая",
		RadiusMeters: &radius,
		BoundaryRule: &rule,
	}})
	if err != nil {
		t.Fatal(err)
	}

	ring := firstGeoJSONRing(t, area.GeoJSON)
	line := knownPushkinBoundaryLine()
	center := geomPoint{Lng: 41.6285588, Lat: 41.64800157}
	centerSide := lineSide(line[0], line[1], center)
	for _, point := range ring {
		side := lineSide(line[0], line[1], geomPoint{Lng: point[0], Lat: point[1]})
		if side*centerSide < -1e-10 {
			t.Fatalf("point should be clipped by Pushkin boundary rule: %+v", point)
		}
	}
	if bounds := ringBounds(ring); bounds.minLat < 41.638 {
		t.Fatalf("expected southern edge to be clipped near Pushkin Street, got bounds %+v", bounds)
	}
}

func TestPostGISGeometryBuilderAppliesSeaDistanceRule(t *testing.T) {
	radius := 1000
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "New Boulevard, Batumi",
			Latitude:  41.6339,
			Longitude: 41.6077,
		}},
		Segments: 24,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:         "Airport area",
		CenterName:   "Airport area",
		RadiusMeters: &radius,
		RawText:      "район аэропорта не далее 1 км от моря",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if area.GeoJSON == "" {
		t.Fatal("expected sea-clipped search area")
	}
	corridor := batumiOneKilometerSeaCorridor()
	for _, point := range firstGeoJSONRing(t, area.GeoJSON) {
		if !pointInConvexPolygon(point, corridor) {
			t.Fatalf("point should be inside sea corridor: %+v", point)
		}
	}
}

func TestPostGISGeometryBuilderIncludesCoastalNewBoulevardArea(t *testing.T) {
	radius := 1000
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "Wrong old boulevard result",
			Latitude:  41.6480,
			Longitude: 41.6285,
		}},
		Segments: 24,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:         "Новый бульвар",
		CenterName:   "Новый бульвар",
		RadiusMeters: &radius,
		RawText:      "Новый бульвар не далее 1 км от моря",
	}})
	if err != nil {
		t.Fatal(err)
	}

	ring := firstGeoJSONRing(t, area.GeoJSON)
	coastalPoints := [][]float64{
		{41.5865, 41.6045},
		{41.5995, 41.6170},
		{41.6105, 41.6260},
	}
	for _, point := range coastalPoints {
		if !pointInConvexPolygon(point, ring) {
			t.Fatalf("expected coastal New Boulevard point to be included: %+v in %+v", point, ringBounds(ring))
		}
	}
	if inlandAirport := []float64{41.6050, 41.5920}; pointInConvexPolygon(inlandAirport, ring) {
		t.Fatalf("expected inland airport-side point to stay outside New Boulevard sea area: %+v", inlandAirport)
	}
}

func TestPostGISGeometryBuilderUsesKnownSixMayParkCenter(t *testing.T) {
	radius := 1000
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "Wrong Batumi center result",
			Latitude:  41.64610,
			Longitude: 41.64049,
		}},
		Segments: 24,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:         "парк 6 мая",
		CenterName:   "парк 6 мая",
		RadiusMeters: &radius,
		RawText:      "около парка 6 мая в радиусе 1 км",
	}})
	if err != nil {
		t.Fatal(err)
	}
	bounds := ringBounds(firstGeoJSONRing(t, area.GeoJSON))
	if bounds.maxLng > 41.641 {
		t.Fatalf("expected known 6 May Park center instead of Batumi center, got bounds %+v", bounds)
	}
	if bounds.minLat < 41.638 || bounds.maxLat > 41.658 {
		t.Fatalf("unexpected 6 May Park latitude bounds %+v", bounds)
	}
}

func TestPostGISGeometryBuilderUsesKnownNewBoulevardCenter(t *testing.T) {
	radius := 1000
	builder := PostGISGeometryBuilder{
		Resolver: fakeGeoResolver{place: &domain.ResolvedPlace{
			Name:      "Wrong old boulevard result",
			Latitude:  41.6480,
			Longitude: 41.6285,
		}},
		Segments: 24,
	}

	area, err := builder.BuildSearchGeometry(context.Background(), []domain.LocationPreference{{
		Name:         "Новый бульвар",
		CenterName:   "Новый бульвар",
		RadiusMeters: &radius,
		RawText:      "Новый бульвар не далее 1 км от моря",
	}})
	if err != nil {
		t.Fatal(err)
	}
	ring := firstGeoJSONRing(t, area.GeoJSON)
	bounds := ringBounds(ring)
	if bounds.maxLat < 41.63 {
		t.Fatalf("expected New Boulevard sea area to reach northern coastal blocks, got bounds %+v", bounds)
	}
	if bounds.minLng > 41.58 || bounds.maxLng < 41.62 {
		t.Fatalf("expected New Boulevard sea area to cover coastal corridor, got bounds %+v", bounds)
	}
}

type fakeGeoResolver struct {
	place  *domain.ResolvedPlace
	street *domain.ResolvedStreet
}

func (r fakeGeoResolver) ResolvePlace(ctx context.Context, query string) (*domain.ResolvedPlace, error) {
	return r.place, nil
}

func (r fakeGeoResolver) ResolveStreet(ctx context.Context, query string) (*domain.ResolvedStreet, error) {
	return r.street, nil
}

func firstGeoJSONRing(t *testing.T, raw string) [][]float64 {
	t.Helper()
	var geo struct {
		Coordinates [][][][]float64 `json:"coordinates"`
	}
	if err := json.Unmarshal([]byte(raw), &geo); err != nil {
		t.Fatal(err)
	}
	if len(geo.Coordinates) == 0 || len(geo.Coordinates[0]) == 0 {
		t.Fatalf("missing coordinates in %s", raw)
	}
	return geo.Coordinates[0][0]
}

func pointInConvexPolygon(point []float64, polygon [][]float64) bool {
	clip := openPolygon(polygon)
	if signedArea(clip) < 0 {
		reversePolygon(clip)
	}
	for i, current := range clip {
		next := clip[(i+1)%len(clip)]
		if lineSide(geomPoint{Lng: current[0], Lat: current[1]}, geomPoint{Lng: next[0], Lat: next[1]}, geomPoint{Lng: point[0], Lat: point[1]}) < -1e-10 {
			return false
		}
	}
	return true
}

type testBounds struct {
	minLng float64
	maxLng float64
	minLat float64
	maxLat float64
}

func ringBounds(ring [][]float64) testBounds {
	bounds := testBounds{minLng: ring[0][0], maxLng: ring[0][0], minLat: ring[0][1], maxLat: ring[0][1]}
	for _, point := range ring[1:] {
		if point[0] < bounds.minLng {
			bounds.minLng = point[0]
		}
		if point[0] > bounds.maxLng {
			bounds.maxLng = point[0]
		}
		if point[1] < bounds.minLat {
			bounds.minLat = point[1]
		}
		if point[1] > bounds.maxLat {
			bounds.maxLat = point[1]
		}
	}
	return bounds
}
