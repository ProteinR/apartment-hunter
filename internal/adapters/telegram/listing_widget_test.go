package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"apartment-hunter/internal/evaluation"
)

func circleRing(centerLon, centerLat, radiusKm float64, points int) [][]float64 {
	ring := make([][]float64, 0, points+1)
	for i := 0; i <= points; i++ {
		angle := 2 * math.Pi * float64(i) / float64(points)
		dLat := radiusKm * math.Sin(angle) / kilometersPerDegreeLatitude
		dLon := radiusKm * math.Cos(angle) / (kilometersPerDegreeEquator * math.Cos(centerLat*math.Pi/180))
		ring = append(ring, []float64{centerLon + dLon, centerLat + dLat})
	}
	return ring
}

func multiPolygonGeoJSON(t *testing.T, rings ...[][]float64) string {
	t.Helper()
	coordinates := make([][][][]float64, 0, len(rings))
	for _, ring := range rings {
		coordinates = append(coordinates, [][][]float64{ring})
	}
	encoded, err := json.Marshal(map[string]any{"type": "MultiPolygon", "coordinates": coordinates})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func realisticListingReport() evaluation.ReportView {
	criteria := []evaluation.ReportCriterion{
		{Label: "Бюджет до $1600", Outcome: evaluation.CriterionMet, Detail: "$1500 в месяц"},
		{Label: "Площадь от 60 м²", Outcome: evaluation.CriterionMet, Detail: "65 м²"},
		{Label: "Посудомойка", Outcome: evaluation.CriterionMet, Detail: "видна на фото кухни"},
		{Label: "Духовка", Outcome: evaluation.CriterionMet, Detail: "упомянута в тексте объявления"},
		{Label: "Район", Outcome: evaluation.CriterionMet, Detail: "Новый бульвар, 400 м от зоны"},
		{Label: "Ремонт", Outcome: evaluation.CriterionFailed, Detail: "глянцевые потолки, темные стены, старый ремонт в спальне"},
		{Label: "Вид из окна", Outcome: evaluation.CriterionFailed, Detail: "окна смотрят на соседний дом"},
		{Label: "Животные", Outcome: evaluation.CriterionSkipped, Detail: "не указано"},
		{Label: "Кондиционер", Outcome: evaluation.CriterionMet, Detail: "в каждой комнате"},
		{Label: "Балкон", Outcome: evaluation.CriterionMet, Detail: "с видом на море"},
		{Label: "Этаж", Outcome: evaluation.CriterionMet, Detail: "14 из 30"},
		{Label: "Парковка", Outcome: evaluation.CriterionSkipped, Detail: "не указано"},
		{Label: "Лифт", Outcome: evaluation.CriterionSkipped, Detail: "не указано"},
	}
	return evaluation.ReportView{
		Title:       "Сдается двухкомнатная квартира 65 м² в новом комплексе на Новом бульваре, свежий ремонт, вся техника, вид на море с 14 этажа, долгосрочно от года",
		HasScore:    true,
		ScoreTotal:  84,
		Status:      "passed",
		StatusLabel: "подходит",
		Price:       1500,
		AreaM2:      65,
		District:    "Новый бульвар",
		Latitude:    41.6380,
		Longitude:   41.6150,
		Criteria:    criteria,
		MessageLink: "https://t.me/domikibatumi/1144629",
		SourceLabel: "@domikibatumi",
	}
}

func twelveDistrictGeoJSON(t *testing.T) string {
	var rings [][][]float64
	for i := 0; i < 12; i++ {
		rings = append(rings, circleRing(41.60+0.012*float64(i%4), 41.62+0.012*float64(i/4), 1.5, 64))
	}
	return multiPolygonGeoJSON(t, rings...)
}

func decodeListingCardData(t *testing.T, html string) listingCardData {
	t.Helper()
	start := strings.Index(html, "const D=")
	if start < 0 {
		t.Fatalf("data assignment missing")
	}
	rest := html[start+len("const D="):]
	end := strings.Index(rest, ",$=")
	if end < 0 {
		t.Fatalf("data assignment end missing")
	}
	var data listingCardData
	if err := json.Unmarshal([]byte(rest[:end]), &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	return data
}

func TestListingCardWidgetFitsSizeLimitWithTwelveDistricts(t *testing.T) {
	html, err := BuildListingCardWidgetHTML(realisticListingReport(), twelveDistrictGeoJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	if size := compressedWidgetSize(html); size > maxWidgetCompressedBytes {
		t.Fatalf("compressed size %d", size)
	}
	if strings.Contains(html, listingCardDataPlaceholder) {
		t.Fatalf("placeholder left in html")
	}
	data := decodeListingCardData(t, html)
	if data.Map == nil || len(data.Map.Zones) != 12 {
		t.Fatalf("map zones = %+v", data.Map)
	}
	if data.Price != 1500 || !data.Passed || data.Verdict != "84 · подходит" {
		t.Fatalf("header data = %+v", data)
	}
	if data.Facts != "65 м² · Новый бульвар · @domikibatumi" {
		t.Fatalf("facts = %q", data.Facts)
	}
	if len(data.Criteria) != maxListingCardCriteria {
		t.Fatalf("criteria rows = %d", len(data.Criteria))
	}
}

func TestListingCardMapPlacesListingInsideItsZone(t *testing.T) {
	report := realisticListingReport()
	report.Latitude, report.Longitude = 41.6300, 41.6100
	geoJSON := multiPolygonGeoJSON(t, circleRing(41.6100, 41.6300, 1.5, 64), circleRing(41.6600, 41.6500, 1, 64))
	data := decodeListingCardData(t, mustBuildListingCard(t, report, geoJSON))
	zone := data.Map.Zones[0]
	minX, maxX, minY, maxY := math.MaxInt, math.MinInt, math.MaxInt, math.MinInt
	for i := 0; i < len(zone); i += 2 {
		minX, maxX = min(minX, zone[i]), max(maxX, zone[i])
		minY, maxY = min(minY, zone[i+1]), max(maxY, zone[i+1])
	}
	centerX, centerY := (minX+maxX)/2, (minY+maxY)/2
	if abs(data.Map.X-centerX) > 2 || abs(data.Map.Y-centerY) > 2 {
		t.Fatalf("listing at %d,%d, zone center %d,%d", data.Map.X, data.Map.Y, centerX, centerY)
	}
	if data.Map.Width != listingCardMapWidth || data.Map.Height > listingCardMapMaxHeight {
		t.Fatalf("map size %dx%d", data.Map.Width, data.Map.Height)
	}
	if data.Map.ScaleBarPx <= 0 || data.Map.ScaleBarPx > listingCardMapMaxScaleBarPx || data.Map.ScaleBarLabel == "" {
		t.Fatalf("scale bar %d %q", data.Map.ScaleBarPx, data.Map.ScaleBarLabel)
	}
	for i := 0; i < len(zone); i += 2 {
		if zone[i] < 0 || zone[i] > data.Map.Width || zone[i+1] < 0 || zone[i+1] > data.Map.Height {
			t.Fatalf("zone point outside map: %d,%d", zone[i], zone[i+1])
		}
	}
}

func TestListingCardWithoutSearchAreaHasNoMap(t *testing.T) {
	for _, geoJSON := range []string{"", "not json", `{"type":"Point","coordinates":[41.6,41.6]}`} {
		data := decodeListingCardData(t, mustBuildListingCard(t, realisticListingReport(), geoJSON))
		if data.Map != nil {
			t.Fatalf("map for %q", geoJSON)
		}
	}
}

func TestListingCardEscapesScriptBreakingText(t *testing.T) {
	report := realisticListingReport()
	report.Title = "</script><script>alert(1)</script>"
	html := mustBuildListingCard(t, report, "")
	if strings.Count(html, "</script>") != 1 {
		t.Fatalf("title broke out of the script element")
	}
	if decodeListingCardData(t, html).Title != report.Title {
		t.Fatalf("title not preserved")
	}
}

func mustBuildListingCard(t *testing.T, report evaluation.ReportView, geoJSON string) string {
	t.Helper()
	html, err := BuildListingCardWidgetHTML(report, geoJSON)
	if err != nil {
		t.Fatal(err)
	}
	return html
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

type fakeCardTelegram struct {
	mu           sync.Mutex
	albumNames   []string
	albumSilent  bool
	widget       map[string]any
	widgetStatus int
	widgetReply  string
}

func (f *fakeCardTelegram) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMediaGroup"):
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			reader := multipart.NewReader(r.Body, params["boundary"])
			for {
				part, err := reader.NextPart()
				if err != nil {
					break
				}
				value, _ := io.ReadAll(part)
				switch part.FormName() {
				case "media":
					var media []map[string]any
					_ = json.Unmarshal(value, &media)
					for _, item := range media {
						f.albumNames = append(f.albumNames, fmt.Sprint(item["media"]))
					}
				case "disable_notification":
					f.albumSilent = string(value) == "true"
				}
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[{"message_id":10},{"message_id":11}]}`)
		case strings.HasSuffix(r.URL.Path, "/sendWidget"):
			_ = json.NewDecoder(r.Body).Decode(&f.widget)
			if f.widgetStatus != 0 {
				w.WriteHeader(f.widgetStatus)
			}
			_, _ = io.WriteString(w, f.widgetReply)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestSendListingCardSendsSilentAlbumWithMapLastThenWidget(t *testing.T) {
	fake := &fakeCardTelegram{widgetReply: `{"ok":true,"result":{"message_id":12}}`}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	photos := [][]byte{[]byte("1"), nil, []byte("2"), []byte("3"), []byte("4"), []byte("5")}
	result, err := sendListingCardNotification(context.Background(), "TOKEN", 42, ListingCardNotification{
		Report:   realisticListingReport(),
		MapImage: []byte("map"),
		Photos:   photos,
		Caption:  "hint",
	}, listingCardEndpoints{apiServerURL: server.URL, widgetEndpoint: server.URL + "/beta/bot%s/sendWidget", client: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if result.MessageID != 12 || !result.AlbumSent || result.AlbumItems != 5 {
		t.Fatalf("result = %+v", result)
	}
	want := []string{"attach://photo1.jpg", "attach://photo2.jpg", "attach://photo3.jpg", "attach://photo4.jpg", "attach://search-area-map.png"}
	if strings.Join(fake.albumNames, ",") != strings.Join(want, ",") || !fake.albumSilent {
		t.Fatalf("album = %v silent=%t", fake.albumNames, fake.albumSilent)
	}
	if fake.widget["caption"] != "hint" || fake.widget["chat_id"] != float64(42) {
		t.Fatalf("widget payload = %v", fake.widget)
	}
	markup, _ := json.Marshal(fake.widget["reply_markup"])
	if !strings.Contains(string(markup), ListingRejectCallbackData) || !strings.Contains(string(markup), "domikibatumi/1144629") {
		t.Fatalf("reply markup = %s", markup)
	}
	if html, _ := fake.widget["html"].(string); !strings.Contains(html, `"p":1500`) {
		t.Fatalf("widget html missing data")
	}
}

func TestSendListingCardReportsRejectedWidgetAfterAlbumWithoutToken(t *testing.T) {
	fake := &fakeCardTelegram{widgetStatus: http.StatusBadRequest, widgetReply: `{"ok":false,"description":"Bad Request: method not found"}`}
	server := httptest.NewServer(fake.handler(t))
	defer server.Close()
	result, err := sendListingCardNotification(context.Background(), "SECRET123", 42, ListingCardNotification{
		Report:   realisticListingReport(),
		MapImage: []byte("map"),
		Photos:   [][]byte{[]byte("1")},
	}, listingCardEndpoints{apiServerURL: server.URL, widgetEndpoint: server.URL + "/beta/bot%s/sendWidget", client: server.Client()})
	if !errors.Is(err, ErrListingCardRejected) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("token leaked: %v", err)
	}
	if !result.AlbumSent || result.MessageID != 0 {
		t.Fatalf("result = %+v", result)
	}
}

func TestListingCardMapKeepsMinimumHeightAndCentersTinyAreas(t *testing.T) {
	report := realisticListingReport()
	report.Latitude, report.Longitude = 41.6300, 41.6100
	wide := multiPolygonGeoJSON(t, circleRing(41.55, 41.63, 0.3, 32), circleRing(41.70, 41.63, 0.3, 32))
	wideMap := decodeListingCardData(t, mustBuildListingCard(t, report, wide)).Map
	if wideMap.Height < listingCardMapMinHeight {
		t.Fatalf("wide map height %d", wideMap.Height)
	}
	tiny := multiPolygonGeoJSON(t, circleRing(41.6100, 41.6300, 0.02, 32))
	tinyMap := decodeListingCardData(t, mustBuildListingCard(t, report, tiny)).Map
	if abs(tinyMap.X-tinyMap.Width/2) > 2 || abs(tinyMap.Y-tinyMap.Height/2) > 2 {
		t.Fatalf("tiny area listing at %d,%d in %dx%d", tinyMap.X, tinyMap.Y, tinyMap.Width, tinyMap.Height)
	}
}
