package telegram

import (
	"bytes"
	"compress/zlib"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"apartment-hunter/internal/evaluation"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

//go:embed widgets/listing_card.html
var listingCardWidgetTemplate string

const (
	listingCardDataPlaceholder  = "__LISTING__"
	maxWidgetCompressedBytes    = 4000
	maxWidgetRawBytes           = 32000
	maxListingCardAlbumPhotos   = 4
	maxListingCardCriteria      = 11
	maxListingCardTitleRunes    = 160
	listingCardMapWidth         = 296
	listingCardMapMaxHeight     = 200
	listingCardMapMinHeight     = 120
	maxListingCardLabelRunes    = 40
	listingCardMapPadding       = 14
	listingCardMapMaxScaleBarPx = 90
	kilometersPerDegreeLatitude = 110.57
	kilometersPerDegreeEquator  = 111.32
	listingCardMarkSkipped      = 0
	listingCardMarkMet          = 1
	listingCardMarkFailed       = 2
)

var listingCardRingPointBudgets = []int{28, 18, 12, 8}

var ErrListingCardRejected = errors.New("telegram rejected the listing card")

type ListingCardNotification struct {
	Report            evaluation.ReportView
	MapImage          []byte
	Photos            [][]byte
	SearchAreaGeoJSON string
	Caption           string
}

type ListingCardResult struct {
	MessageID  int
	AlbumSent  bool
	AlbumItems int
	AlbumError string
}

type listingCardData struct {
	Price    int             `json:"p"`
	Verdict  string          `json:"v"`
	Passed   bool            `json:"ok"`
	Facts    string          `json:"f"`
	Title    string          `json:"t,omitempty"`
	Criteria [][]any         `json:"c"`
	Map      *listingCardMap `json:"m,omitempty"`
}

type listingCardMap struct {
	Width         int     `json:"w"`
	Height        int     `json:"h"`
	Zones         [][]int `json:"z"`
	X             int     `json:"x"`
	Y             int     `json:"y"`
	ScaleBarPx    int     `json:"l"`
	ScaleBarLabel string  `json:"s"`
}

type listingCardEndpoints struct {
	apiServerURL   string
	widgetEndpoint string
	client         *http.Client
}

func SendListingCardNotification(ctx context.Context, token string, chatID int64, notification ListingCardNotification) (ListingCardResult, error) {
	return sendListingCardNotification(ctx, token, chatID, notification, listingCardEndpoints{
		widgetEndpoint: defaultWidgetEndpoint,
		client:         &http.Client{Timeout: 60 * time.Second},
	})
}

func sendListingCardNotification(ctx context.Context, token string, chatID int64, notification ListingCardNotification, endpoints listingCardEndpoints) (ListingCardResult, error) {
	var result ListingCardResult
	html, err := BuildListingCardWidgetHTML(notification.Report, notification.SearchAreaGeoJSON)
	if err != nil {
		return result, fmt.Errorf("%w: %v", ErrListingCardRejected, err)
	}
	album := listingCardAlbum(notification)
	if len(album) > 0 {
		if err := sendListingCardAlbum(ctx, token, chatID, album, endpoints); err != nil {
			if ctx.Err() != nil {
				return result, fmt.Errorf("telegram album: %s", redactBotToken(err.Error(), token))
			}
			result.AlbumError = redactBotToken(err.Error(), token)
		} else {
			result.AlbumSent = true
			result.AlbumItems = len(album)
		}
	}
	messageID, err := postListingCardWidget(ctx, token, chatID, html, notification, endpoints)
	if err != nil {
		if ctx.Err() != nil {
			return result, err
		}
		return result, fmt.Errorf("%w: %s", ErrListingCardRejected, redactBotToken(err.Error(), token))
	}
	result.MessageID = messageID
	return result, nil
}

type listingCardAlbumItem struct {
	name string
	data []byte
}

func listingCardAlbum(notification ListingCardNotification) []listingCardAlbumItem {
	var items []listingCardAlbumItem
	for _, photo := range notification.Photos {
		if len(photo) == 0 {
			continue
		}
		if len(items) == maxListingCardAlbumPhotos {
			break
		}
		items = append(items, listingCardAlbumItem{name: fmt.Sprintf("photo%d.jpg", len(items)+1), data: photo})
	}
	if len(notification.MapImage) > 0 {
		items = append(items, listingCardAlbumItem{name: "search-area-map.png", data: notification.MapImage})
	}
	return items
}

func sendListingCardAlbum(ctx context.Context, token string, chatID int64, items []listingCardAlbumItem, endpoints listingCardEndpoints) error {
	options := []telegrambot.Option{telegrambot.WithSkipGetMe()}
	if endpoints.client != nil {
		options = append(options, telegrambot.WithHTTPClient(60*time.Second, endpoints.client))
	}
	if endpoints.apiServerURL != "" {
		options = append(options, telegrambot.WithServerURL(endpoints.apiServerURL))
	}
	api, err := telegrambot.New(token, options...)
	if err != nil {
		return err
	}
	if len(items) == 1 {
		_, err := api.SendPhoto(ctx, &telegrambot.SendPhotoParams{
			ChatID:              chatID,
			Photo:               &models.InputFileUpload{Filename: items[0].name, Data: bytes.NewReader(items[0].data)},
			DisableNotification: true,
		})
		return err
	}
	media := make([]models.InputMedia, 0, len(items))
	for _, item := range items {
		media = append(media, &models.InputMediaPhoto{
			Media:           "attach://" + item.name,
			MediaAttachment: bytes.NewReader(item.data),
		})
	}
	_, err = api.SendMediaGroup(ctx, &telegrambot.SendMediaGroupParams{
		ChatID:              chatID,
		Media:               media,
		DisableNotification: true,
	})
	return err
}

func postListingCardWidget(ctx context.Context, token string, chatID int64, html string, notification ListingCardNotification, endpoints listingCardEndpoints) (int, error) {
	payload := map[string]any{
		"chat_id": chatID,
		"html":    html,
	}
	if caption := strings.TrimSpace(notification.Caption); caption != "" {
		payload["caption"] = caption
	}
	if markup := ListingNotificationKeyboard(notification.Report); markup != nil {
		payload["reply_markup"] = markup
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	endpoint := endpoints.widgetEndpoint
	if endpoint == "" {
		endpoint = defaultWidgetEndpoint
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(endpoint, token), bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := endpoints.client
	if client == nil {
		client = http.DefaultClient
	}
	var message struct {
		MessageID int `json:"message_id"`
	}
	if err := callTelegramJSON(client, request, &message); err != nil {
		return 0, err
	}
	if message.MessageID == 0 {
		return 0, fmt.Errorf("telegram sendWidget returned no message id")
	}
	return message.MessageID, nil
}

func BuildListingCardWidgetHTML(report evaluation.ReportView, searchAreaGeoJSON string) (string, error) {
	zones := searchAreaRings(searchAreaGeoJSON)
	data := listingCardData{
		Price:    report.Price,
		Verdict:  listingCardVerdict(report),
		Passed:   report.Passed(),
		Facts:    listingCardFacts(report),
		Title:    truncateRunes(strings.TrimSpace(report.Title), maxListingCardTitleRunes),
		Criteria: listingCardCriteria(report.Criteria, true),
	}
	for _, budget := range listingCardRingPointBudgets {
		data.Map = buildListingCardMap(zones, report.Latitude, report.Longitude, budget)
		if html, ok := renderListingCardWidget(data); ok {
			return html, nil
		}
	}
	data.Criteria = listingCardCriteria(report.Criteria, false)
	if html, ok := renderListingCardWidget(data); ok {
		return html, nil
	}
	data.Map = nil
	data.Title = ""
	if html, ok := renderListingCardWidget(data); ok {
		return html, nil
	}
	return "", fmt.Errorf("listing card widget exceeds %d compressed bytes", maxWidgetCompressedBytes)
}

func renderListingCardWidget(data listingCardData) (string, bool) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return "", false
	}
	html := strings.Replace(listingCardWidgetTemplate, listingCardDataPlaceholder, string(encoded), 1)
	if len(html) > maxWidgetRawBytes {
		return "", false
	}
	return html, compressedWidgetSize(html) <= maxWidgetCompressedBytes
}

func compressedWidgetSize(html string) int {
	var buffer bytes.Buffer
	writer, err := zlib.NewWriterLevel(&buffer, zlib.BestCompression)
	if err != nil {
		return math.MaxInt
	}
	_, _ = writer.Write([]byte(html))
	if err := writer.Close(); err != nil {
		return math.MaxInt
	}
	return buffer.Len()
}

func listingCardVerdict(report evaluation.ReportView) string {
	label := verdictLabel(report)
	if report.HasScore {
		return fmt.Sprintf("%.0f · %s", report.ScoreTotal, label)
	}
	return label
}

func listingCardFacts(report evaluation.ReportView) string {
	var facts []string
	if report.AreaM2 > 0 {
		facts = append(facts, formatArea(report.AreaM2)+" м²")
	}
	if report.District != "" {
		facts = append(facts, truncateRunes(report.District, maxListingCardLabelRunes))
	}
	if report.SourceLabel != "" {
		facts = append(facts, truncateRunes(report.SourceLabel, maxListingCardLabelRunes))
	}
	return strings.Join(facts, " · ")
}

func listingCardCriteria(criteria []evaluation.ReportCriterion, withDetails bool) [][]any {
	rows := [][]any{}
	for _, criterion := range criteria {
		if len(rows) == maxListingCardCriteria {
			break
		}
		row := []any{truncateRunes(criterion.Label, maxListingCardLabelRunes), listingCardMark(criterion.Outcome)}
		if withDetails && criterion.Detail != "" {
			row = append(row, truncateRunes(criterion.Detail, maxListingCardLabelRunes))
		}
		rows = append(rows, row)
	}
	return rows
}

func listingCardMark(outcome evaluation.CriterionOutcome) int {
	switch outcome {
	case evaluation.CriterionMet:
		return listingCardMarkMet
	case evaluation.CriterionFailed:
		return listingCardMarkFailed
	default:
		return listingCardMarkSkipped
	}
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

type geoPoint struct {
	lon float64
	lat float64
}

func searchAreaRings(geoJSON string) [][]geoPoint {
	geoJSON = strings.TrimSpace(geoJSON)
	if geoJSON == "" {
		return nil
	}
	var geometry struct {
		Type        string          `json:"type"`
		Coordinates json.RawMessage `json:"coordinates"`
	}
	if err := json.Unmarshal([]byte(geoJSON), &geometry); err != nil {
		return nil
	}
	var polygons [][][][]float64
	switch geometry.Type {
	case "MultiPolygon":
		if err := json.Unmarshal(geometry.Coordinates, &polygons); err != nil {
			return nil
		}
	case "Polygon":
		var polygon [][][]float64
		if err := json.Unmarshal(geometry.Coordinates, &polygon); err != nil {
			return nil
		}
		polygons = [][][][]float64{polygon}
	default:
		return nil
	}
	var rings [][]geoPoint
	for _, polygon := range polygons {
		if len(polygon) == 0 {
			continue
		}
		var ring []geoPoint
		for _, position := range polygon[0] {
			if len(position) < 2 {
				continue
			}
			ring = append(ring, geoPoint{lon: position[0], lat: position[1]})
		}
		if len(ring) >= 3 {
			rings = append(rings, ring)
		}
	}
	return rings
}

func buildListingCardMap(rings [][]geoPoint, latitude, longitude float64, ringPointBudget int) *listingCardMap {
	if len(rings) == 0 {
		return nil
	}
	hasListing := latitude != 0 || longitude != 0
	referenceLatitude := rings[0][0].lat
	if hasListing {
		referenceLatitude = latitude
	}
	kilometersPerDegreeLongitude := kilometersPerDegreeEquator * math.Cos(referenceLatitude*math.Pi/180)
	project := func(point geoPoint) (float64, float64) {
		return point.lon * kilometersPerDegreeLongitude, -point.lat * kilometersPerDegreeLatitude
	}
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	include := func(x, y float64) {
		minX, maxX = math.Min(minX, x), math.Max(maxX, x)
		minY, maxY = math.Min(minY, y), math.Max(maxY, y)
	}
	for _, ring := range rings {
		for _, point := range ring {
			include(project(point))
		}
	}
	if hasListing {
		include(project(geoPoint{lon: longitude, lat: latitude}))
	}
	spanX := math.Max(maxX-minX, 0.2)
	spanY := math.Max(maxY-minY, 0.2)
	drawWidth := float64(listingCardMapWidth - 2*listingCardMapPadding)
	drawHeight := float64(listingCardMapMaxHeight - 2*listingCardMapPadding)
	scale := math.Min(drawWidth/spanX, drawHeight/spanY)
	height := max(int(math.Ceil(spanY*scale))+2*listingCardMapPadding, listingCardMapMinHeight)
	centerX, centerY := (minX+maxX)/2, (minY+maxY)/2
	toPixels := func(point geoPoint) (int, int) {
		x, y := project(point)
		return int(math.Round(float64(listingCardMapWidth)/2 + (x-centerX)*scale)), int(math.Round(float64(height)/2 + (y-centerY)*scale))
	}
	result := &listingCardMap{Width: listingCardMapWidth, Height: height}
	for _, ring := range rings {
		step := int(math.Ceil(float64(len(ring)) / float64(ringPointBudget)))
		if step < 1 {
			step = 1
		}
		var zone []int
		lastX, lastY := math.MinInt, math.MinInt
		for i := 0; i < len(ring); i += step {
			x, y := toPixels(ring[i])
			if x == lastX && y == lastY {
				continue
			}
			zone = append(zone, x, y)
			lastX, lastY = x, y
		}
		if len(zone) >= 6 {
			result.Zones = append(result.Zones, zone)
		}
	}
	if len(result.Zones) == 0 {
		return nil
	}
	if hasListing {
		result.X, result.Y = toPixels(geoPoint{lon: longitude, lat: latitude})
	} else {
		result.X, result.Y = -20, -20
	}
	result.ScaleBarPx, result.ScaleBarLabel = listingCardScaleBar(scale)
	return result
}

func listingCardScaleBar(pixelsPerKilometer float64) (int, string) {
	steps := []struct {
		kilometers float64
		label      string
	}{
		{10, "10 км"}, {5, "5 км"}, {2, "2 км"}, {1, "1 км"}, {0.5, "500 м"}, {0.2, "200 м"}, {0.1, "100 м"},
	}
	for _, step := range steps {
		pixels := step.kilometers * pixelsPerKilometer
		if pixels <= listingCardMapMaxScaleBarPx {
			return int(math.Round(pixels)), step.label
		}
	}
	last := steps[len(steps)-1]
	return int(math.Round(last.kilometers * pixelsPerKilometer)), last.label
}

func redactBotToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "<token>")
}
