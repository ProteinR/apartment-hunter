package googlemaps

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"apartment-hunter/internal/cities"
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

func TestGeocoderForTbilisiAcceptsTbilisiResultAndAppendsTbilisiRegion(t *testing.T) {
	var requestedAddress string
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requestedAddress = r.URL.Query().Get("address")
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

	base := NewGeocoder("test-key", "Batumi, Georgia")
	base.Client = &http.Client{Transport: transport}
	geocoder := base.ForCity(cities.Tbilisi)

	loc, err := geocoder.Resolve(context.Background(), "Lermontov Street 14", 25000)
	if err != nil {
		t.Fatal(err)
	}
	if requestedAddress != "Lermontov Street 14, Tbilisi, Georgia" {
		t.Fatalf("unexpected requested address %q", requestedAddress)
	}
	if loc.Latitude == 0 || loc.Longitude == 0 {
		t.Fatalf("expected Tbilisi result to be accepted, got %+v", loc)
	}
	if base.DefaultRegion != "Batumi, Georgia" {
		t.Fatalf("ForCity must not mutate the base geocoder, got %q", base.DefaultRegion)
	}
}

func TestGeocoderKeepsQueryThatNamesACity(t *testing.T) {
	geocoder := NewGeocoder("test-key", "Batumi, Georgia")
	if got := geocoder.RequestQuery("Vake Tbilisi"); got != "Vake Tbilisi" {
		t.Fatalf("unexpected query %q", got)
	}
	if got := geocoder.RequestQuery("Vake Park"); got != "Vake Park, Batumi, Georgia" {
		t.Fatalf("unexpected query %q", got)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
