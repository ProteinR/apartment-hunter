package googlemaps

import (
	"net/url"
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
)

const testSearchAreaGeoJSON = `{"type":"MultiPolygon","coordinates":[[[[41.620000,41.640000],[41.630000,41.640000],[41.630000,41.650000],[41.620000,41.650000],[41.620000,41.640000]]]]}`

func TestStaticMapRendererBuildsSearchAreaMapURL(t *testing.T) {
	renderer := StaticMapRenderer{APIKey: "test-key"}

	rawURL := renderer.ListingSearchAreaURL(domain.ApartmentListing{
		Latitude:  41.6466,
		Longitude: 41.6266,
	}, testSearchAreaGeoJSON)
	if rawURL == "" {
		t.Fatal("expected static map URL")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("key") != "test-key" {
		t.Fatalf("unexpected key %q", query.Get("key"))
	}
	if query.Get("size") != "640x640" {
		t.Fatalf("unexpected size %q", query.Get("size"))
	}
	if marker := query.Get("markers"); !strings.Contains(marker, "41.646600,41.626600") {
		t.Fatalf("marker missing listing coordinates: %q", marker)
	}
	paths := query["path"]
	if len(paths) != 1 {
		t.Fatalf("expected one path, got %d", len(paths))
	}
	if !strings.Contains(paths[0], "fillcolor:0x22AA2244") || !strings.Contains(paths[0], "color:0x118811CC") {
		t.Fatalf("path missing green style: %q", paths[0])
	}
	if !strings.Contains(paths[0], "41.640000,41.620000") {
		t.Fatalf("path missing polygon coordinate: %q", paths[0])
	}
}

func TestStaticMapRendererBuildsAreaOnlyMapURL(t *testing.T) {
	renderer := StaticMapRenderer{APIKey: "test-key"}

	rawURL := renderer.SearchAreaURL(testSearchAreaGeoJSON)
	if rawURL == "" {
		t.Fatal("expected static map URL")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("markers") != "" {
		t.Fatalf("expected no listing marker, got %q", query.Get("markers"))
	}
	paths := query["path"]
	if len(paths) != 1 {
		t.Fatalf("expected one path, got %d", len(paths))
	}
	if !strings.Contains(paths[0], "41.640000,41.620000") {
		t.Fatalf("path missing polygon coordinate: %q", paths[0])
	}
}

func TestStaticMapRendererReturnsEmptyWithoutInputs(t *testing.T) {
	renderer := StaticMapRenderer{APIKey: "test-key"}
	listing := domain.ApartmentListing{Latitude: 41.6466, Longitude: 41.6266}

	if got := (StaticMapRenderer{}).ListingSearchAreaURL(listing, testSearchAreaGeoJSON); got != "" {
		t.Fatalf("expected no map without API key, got %q", got)
	}
	if got := renderer.ListingSearchAreaURL(domain.ApartmentListing{}, testSearchAreaGeoJSON); got != "" {
		t.Fatalf("expected no map without listing coordinates, got %q", got)
	}
	if got := renderer.ListingSearchAreaURL(listing, ""); got == "" {
		t.Fatalf("expected map URL without search area when coordinates are present")
	} else if !strings.Contains(got, "zoom=15") || !strings.Contains(got, "markers=") {
		t.Fatalf("expected map URL to have zoom and markers, got %q", got)
	}
	if got := renderer.SearchAreaURL(""); got != "" {
		t.Fatalf("expected no map without search area and no listing coordinates, got %q", got)
	}
	if got := renderer.ListingSearchAreaURL(listing, `{"type":"Point","coordinates":[41.62,41.64]}`); got == "" {
		t.Fatalf("expected map URL for unsupported/invalid GeoJSON when coordinates are present")
	} else if !strings.Contains(got, "zoom=15") || !strings.Contains(got, "markers=") {
		t.Fatalf("expected map URL to fall back to zoom and markers, got %q", got)
	}
}

func TestStaticMapRendererSimplifiesLongPolygons(t *testing.T) {
	renderer := StaticMapRenderer{APIKey: "test-key", MaxURLLen: 900}

	rawURL := renderer.ListingSearchAreaURL(domain.ApartmentListing{
		Latitude:  41.6466,
		Longitude: 41.6266,
	}, largeSearchAreaGeoJSON())
	if rawURL == "" {
		t.Fatal("expected simplified static map URL")
	}
	if len(rawURL) > 900 {
		t.Fatalf("expected URL to be simplified below limit, got length %d", len(rawURL))
	}
}

func largeSearchAreaGeoJSON() string {
	coords := make([]string, 0, 81)
	for i := 0; i < 80; i++ {
		lat := 41.640000 + float64(i%20)*0.0001
		lng := 41.620000 + float64(i)*0.0001
		coords = append(coords, "["+formatCoord(lng)+","+formatCoord(lat)+"]")
	}
	coords = append(coords, coords[0])
	return `{"type":"MultiPolygon","coordinates":[[[` + strings.Join(coords, ",") + `]]]}`
}
