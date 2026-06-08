package googlemaps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGeocoderResolvesFirstResult(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/maps/api/geocode/json" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "test-key" {
			t.Fatalf("missing api key")
		}
		if !strings.Contains(r.URL.Query().Get("address"), "Europe Square") {
			t.Fatalf("unexpected address %q", r.URL.Query().Get("address"))
		}
		body := `{
			"status": "OK",
			"results": [{
				"formatted_address": "Europe Square, Batumi, Georgia",
				"geometry": {"location": {"lat": 41.6511, "lng": 41.6367}}
			}]
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})

	geocoder := NewGeocoder("test-key", "Batumi, Georgia")
	geocoder.Client = &http.Client{Transport: transport}

	loc, err := geocoder.Resolve(context.Background(), "Europe Square", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Name != "Europe Square, Batumi, Georgia" {
		t.Fatalf("unexpected name %q", loc.Name)
	}
	if loc.Latitude != 41.6511 || loc.Longitude != 41.6367 {
		t.Fatalf("unexpected coordinates %.6f %.6f", loc.Latitude, loc.Longitude)
	}
	if loc.MaxDistanceMeters != 1000 {
		t.Fatalf("unexpected distance %d", loc.MaxDistanceMeters)
	}
}

func TestGeocoderRejectsResultOutsideDefaultRegionRadius(t *testing.T) {
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := `{
			"status": "OK",
			"results": [{
				"formatted_address": "Lermontov Street 14, Tbilisi, Georgia",
				"geometry": {"location": {"lat": 41.7151, "lng": 44.8271}}
			}]
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	})

	geocoder := NewGeocoder("test-key", "Batumi, Georgia")
	geocoder.Client = &http.Client{Transport: transport}

	loc, err := geocoder.Resolve(context.Background(), "ул. Лермонтова, 14, Batumi, Georgia", 15000)
	if err != nil {
		t.Fatal(err)
	}
	if loc.Latitude != 0 || loc.Longitude != 0 || loc.Name != "" {
		t.Fatalf("expected out-of-radius result to be ignored, got %+v", loc)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
