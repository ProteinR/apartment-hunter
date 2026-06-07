package googlemaps

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
)

type Geocoder struct {
	APIKey        string
	DefaultRegion string
	Language      string
	Endpoint      string
	Client        *http.Client
}

func NewGeocoder(apiKey, defaultRegion string) *Geocoder {
	return &Geocoder{
		APIKey:        strings.TrimSpace(apiKey),
		DefaultRegion: strings.TrimSpace(defaultRegion),
		Language:      "en",
		Client:        &http.Client{Timeout: 10 * time.Second},
	}
}

func (g *Geocoder) Resolve(ctx context.Context, query string, maxDistanceMeters int) (domain.LocationPreference, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return domain.LocationPreference{}, nil
	}
	if loc, ok, err := geocoding.ResolveCoordinates(query, maxDistanceMeters); ok {
		return loc, err
	}
	if strings.TrimSpace(g.APIKey) == "" {
		return domain.LocationPreference{}, nil
	}

	response, err := g.geocode(ctx, g.queryWithRegion(query))
	if err != nil {
		return domain.LocationPreference{}, err
	}
	if response.Status == "ZERO_RESULTS" {
		return domain.LocationPreference{}, nil
	}
	if response.Status != "OK" {
		if response.ErrorMessage != "" {
			return domain.LocationPreference{}, fmt.Errorf("google geocoding %s: %s", response.Status, response.ErrorMessage)
		}
		return domain.LocationPreference{}, fmt.Errorf("google geocoding %s", response.Status)
	}
	if len(response.Results) == 0 {
		return domain.LocationPreference{}, nil
	}

	result := response.Results[0]
	name := strings.TrimSpace(result.FormattedAddress)
	if name == "" {
		name = query
	}
	return domain.LocationPreference{
		Name:              name,
		MaxDistanceMeters: maxDistanceMeters,
		Latitude:          result.Geometry.Location.Lat,
		Longitude:         result.Geometry.Location.Lng,
	}, nil
}

func (g *Geocoder) ResolvePlace(ctx context.Context, query string) (*domain.ResolvedPlace, error) {
	loc, err := g.Resolve(ctx, query, 0)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(loc.Name) == "" || (loc.Latitude == 0 && loc.Longitude == 0) {
		return nil, nil
	}
	return &domain.ResolvedPlace{
		Name:       loc.Name,
		Latitude:   loc.Latitude,
		Longitude:  loc.Longitude,
		Confidence: 0.8,
	}, nil
}

func (g *Geocoder) ResolveStreet(ctx context.Context, query string) (*domain.ResolvedStreet, error) {
	place, err := g.ResolvePlace(ctx, query)
	if err != nil || place == nil {
		return nil, err
	}
	return &domain.ResolvedStreet{
		Name:       place.Name,
		Latitude:   place.Latitude,
		Longitude:  place.Longitude,
		Confidence: place.Confidence,
	}, nil
}

func (g *Geocoder) queryWithRegion(query string) string {
	if g.DefaultRegion == "" || strings.Contains(query, ",") {
		return query
	}
	return query + ", " + g.DefaultRegion
}

func (g *Geocoder) geocode(ctx context.Context, query string) (geocodeResponse, error) {
	endpointURL := g.Endpoint
	if endpointURL == "" {
		endpointURL = "https://maps.googleapis.com/maps/api/geocode/json"
	}
	endpoint, err := url.Parse(endpointURL)
	if err != nil {
		return geocodeResponse{}, err
	}
	params := endpoint.Query()
	params.Set("address", query)
	params.Set("key", g.APIKey)
	if g.Language != "" {
		params.Set("language", g.Language)
	}
	endpoint.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return geocodeResponse{}, err
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return geocodeResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return geocodeResponse{}, fmt.Errorf("google geocoding http status %d", resp.StatusCode)
	}

	var decoded geocodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return geocodeResponse{}, err
	}
	return decoded, nil
}

func (g *Geocoder) client() *http.Client {
	if g.Client != nil {
		return g.Client
	}
	return &http.Client{Timeout: 10 * time.Second}
}

type geocodeResponse struct {
	Status       string          `json:"status"`
	ErrorMessage string          `json:"error_message"`
	Results      []geocodeResult `json:"results"`
}

type geocodeResult struct {
	FormattedAddress string `json:"formatted_address"`
	Geometry         struct {
		Location struct {
			Lat float64 `json:"lat"`
			Lng float64 `json:"lng"`
		} `json:"location"`
	} `json:"geometry"`
}
