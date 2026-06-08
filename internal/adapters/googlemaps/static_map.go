package googlemaps

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"apartment-hunter/internal/domain"
)

const staticMapsEndpoint = "https://maps.googleapis.com/maps/api/staticmap"

type StaticMapRenderer struct {
	APIKey    string
	Endpoint  string
	Size      string
	MapType   string
	MaxURLLen int
	Client    *http.Client
}

func (r StaticMapRenderer) ListingSearchAreaURL(listing domain.ApartmentListing, searchAreaGeoJSON string) string {
	if strings.TrimSpace(r.APIKey) == "" || strings.TrimSpace(searchAreaGeoJSON) == "" {
		return ""
	}
	if listing.Latitude == 0 && listing.Longitude == 0 {
		return ""
	}
	polygons, ok := parseMultiPolygon(searchAreaGeoJSON)
	if !ok || len(polygons) == 0 {
		return ""
	}

	endpoint := strings.TrimSpace(r.Endpoint)
	if endpoint == "" {
		endpoint = staticMapsEndpoint
	}
	size := strings.TrimSpace(r.Size)
	if size == "" {
		size = "640x640"
	}
	mapType := strings.TrimSpace(r.MapType)
	if mapType == "" {
		mapType = "roadmap"
	}
	maxURLLen := r.MaxURLLen
	if maxURLLen <= 0 {
		maxURLLen = 8000
	}

	for step := 1; step <= 64; step *= 2 {
		values := url.Values{}
		values.Set("key", strings.TrimSpace(r.APIKey))
		values.Set("size", size)
		values.Set("scale", "2")
		values.Set("maptype", mapType)
		values.Set("markers", "color:red|label:A|"+formatCoord(listing.Latitude)+","+formatCoord(listing.Longitude))
		for _, polygon := range polygons {
			path := staticMapPath(polygon, step)
			if path != "" {
				values.Add("path", path)
			}
		}
		if len(values["path"]) == 0 {
			return ""
		}
		rawURL := endpoint + "?" + values.Encode()
		if len(rawURL) <= maxURLLen {
			return rawURL
		}
	}
	return ""
}

func (r StaticMapRenderer) ListingSearchAreaImage(ctx context.Context, listing domain.ApartmentListing, searchAreaGeoJSON string) ([]byte, error) {
	mapURL := r.ListingSearchAreaURL(listing, searchAreaGeoJSON)
	if mapURL == "" {
		return nil, nil
	}
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, mapURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("google static maps returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("google static maps returned empty image")
	}
	return data, nil
}

type multiPolygonGeoJSON struct {
	Type        string          `json:"type"`
	Coordinates [][][][]float64 `json:"coordinates"`
}

func parseMultiPolygon(raw string) ([][][]float64, bool) {
	var geo multiPolygonGeoJSON
	if err := json.Unmarshal([]byte(raw), &geo); err != nil {
		return nil, false
	}
	if !strings.EqualFold(geo.Type, "MultiPolygon") {
		return nil, false
	}
	var polygons [][][]float64
	for _, polygon := range geo.Coordinates {
		if len(polygon) == 0 || len(polygon[0]) < 3 {
			continue
		}
		var ring [][]float64
		for _, coord := range polygon[0] {
			if len(coord) < 2 {
				continue
			}
			lng, lat := coord[0], coord[1]
			if lat < -90 || lat > 90 || lng < -180 || lng > 180 {
				continue
			}
			ring = append(ring, []float64{lat, lng})
		}
		if len(ring) >= 3 {
			polygons = append(polygons, ring)
		}
	}
	return polygons, len(polygons) > 0
}

func staticMapPath(ring [][]float64, step int) string {
	if step <= 0 {
		step = 1
	}
	var parts []string
	parts = append(parts, "fillcolor:0x22AA2244", "color:0x118811CC", "weight:2")
	for i, coord := range ring {
		if i != 0 && i != len(ring)-1 && i%step != 0 {
			continue
		}
		parts = append(parts, formatCoord(coord[0])+","+formatCoord(coord[1]))
	}
	if len(parts) < 6 {
		return ""
	}
	return strings.Join(parts, "|")
}

func formatCoord(value float64) string {
	return strconv.FormatFloat(value, 'f', 6, 64)
}
