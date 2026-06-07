package preferenceparser

import (
	"context"
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
	if len(area.UnresolvedRules) != 1 || !strings.Contains(area.UnresolvedRules[0], "requires_manual_boundary_resolution") {
		t.Fatalf("expected unresolved boundary rule, got %+v", area.UnresolvedRules)
	}
}

type fakeGeoResolver struct {
	place *domain.ResolvedPlace
}

func (r fakeGeoResolver) ResolvePlace(ctx context.Context, query string) (*domain.ResolvedPlace, error) {
	return r.place, nil
}

func (r fakeGeoResolver) ResolveStreet(ctx context.Context, query string) (*domain.ResolvedStreet, error) {
	return nil, nil
}
