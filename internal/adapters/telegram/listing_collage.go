package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"apartment-hunter/internal/evaluation"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	xdraw "golang.org/x/image/draw"
)

const (
	ListingCollageMapSize         = "600x400"
	listingCollageWidth           = 1200
	listingCollageHeight          = 800
	listingCollageMainWidth       = 800
	listingCollageGap             = 4
	listingCollageSidePhotos      = 2
	listingCollageJPEGQuality     = 85
	maxListingCaptionUnits        = 1024
	maxListingCaptionTitleRunes   = 140
	maxListingCaptionFailedLines  = 4
	maxListingCaptionDetailRunes  = 60
	listingCollageFileName        = "listing.jpg"
	listingCollageBackgroundShade = 24
)

var ErrListingCollageRejected = errors.New("telegram rejected the listing collage")

type ListingCollageNotification struct {
	Report   evaluation.ReportView
	MapImage []byte
	Photos   [][]byte
	Hint     string
}

type listingCollageEndpoints struct {
	apiServerURL string
	client       *http.Client
}

func SendListingCollageNotification(ctx context.Context, token string, chatID int64, notification ListingCollageNotification) (int, error) {
	return sendListingCollageNotification(ctx, token, chatID, notification, listingCollageEndpoints{
		client: &http.Client{Timeout: 60 * time.Second},
	})
}

func sendListingCollageNotification(ctx context.Context, token string, chatID int64, notification ListingCollageNotification, endpoints listingCollageEndpoints) (int, error) {
	options := []telegrambot.Option{telegrambot.WithSkipGetMe()}
	if endpoints.client != nil {
		options = append(options, telegrambot.WithHTTPClient(60*time.Second, endpoints.client))
	}
	if endpoints.apiServerURL != "" {
		options = append(options, telegrambot.WithServerURL(endpoints.apiServerURL))
	}
	api, err := telegrambot.New(token, options...)
	if err != nil {
		return 0, err
	}
	caption := BuildListingCaption(notification.Report, notification.Hint)
	markup := ListingNotificationKeyboard(notification.Report)
	collage, err := BuildListingCollage(notification.Photos, notification.MapImage)
	var message *models.Message
	if err == nil {
		params := &telegrambot.SendPhotoParams{
			ChatID:    chatID,
			Photo:     &models.InputFileUpload{Filename: listingCollageFileName, Data: bytes.NewReader(collage)},
			Caption:   caption,
			ParseMode: models.ParseModeHTML,
		}
		if markup != nil {
			params.ReplyMarkup = markup
		}
		message, err = api.SendPhoto(ctx, params)
	} else {
		params := &telegrambot.SendMessageParams{
			ChatID:             chatID,
			Text:               caption,
			ParseMode:          models.ParseModeHTML,
			LinkPreviewOptions: &models.LinkPreviewOptions{IsDisabled: telegrambot.True()},
		}
		if markup != nil {
			params.ReplyMarkup = markup
		}
		message, err = api.SendMessage(ctx, params)
	}
	if err != nil {
		redacted := redactBotToken(err.Error(), token)
		if errors.Is(err, telegrambot.ErrorBadRequest) {
			return 0, fmt.Errorf("%w: %s", ErrListingCollageRejected, redacted)
		}
		return 0, errors.New(redacted)
	}
	return message.ID, nil
}

var errNoCollageImages = errors.New("no images for the collage")

func BuildListingCollage(photos [][]byte, mapImage []byte) ([]byte, error) {
	var decoded []image.Image
	for _, data := range photos {
		if len(decoded) == 1+listingCollageSidePhotos {
			break
		}
		if img := decodeCollageImage(data); img != nil {
			decoded = append(decoded, img)
		}
	}
	mapTile := decodeCollageImage(mapImage)
	if len(decoded) == 0 && mapTile == nil {
		return nil, errNoCollageImages
	}
	canvas := image.NewRGBA(image.Rect(0, 0, listingCollageWidth, listingCollageHeight))
	background := color.RGBA{listingCollageBackgroundShade, listingCollageBackgroundShade, listingCollageBackgroundShade, 255}
	xdraw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: background}, image.Point{}, xdraw.Src)
	if len(decoded) == 0 {
		drawCover(canvas, canvas.Bounds(), mapTile)
		return encodeCollage(canvas)
	}
	side := append([]image.Image{}, decoded[1:]...)
	if mapTile != nil {
		side = append(side, mapTile)
	}
	if len(side) == 0 {
		drawCover(canvas, canvas.Bounds(), decoded[0])
		return encodeCollage(canvas)
	}
	drawCover(canvas, image.Rect(0, 0, listingCollageMainWidth, listingCollageHeight), decoded[0])
	left := listingCollageMainWidth + listingCollageGap
	tileHeight := (listingCollageHeight - listingCollageGap*(len(side)-1)) / len(side)
	for i, tile := range side {
		top := i * (tileHeight + listingCollageGap)
		bottom := top + tileHeight
		if i == len(side)-1 {
			bottom = listingCollageHeight
		}
		target := image.Rect(left, top, listingCollageWidth, bottom)
		if tile == mapTile {
			drawContain(canvas, target, tile)
			continue
		}
		drawCover(canvas, target, tile)
	}
	return encodeCollage(canvas)
}

func decodeCollageImage(data []byte) image.Image {
	if len(data) == 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil || img.Bounds().Dx() == 0 || img.Bounds().Dy() == 0 {
		return nil
	}
	return img
}

func drawCover(canvas *image.RGBA, target image.Rectangle, source image.Image) {
	bounds := source.Bounds()
	targetRatio := float64(target.Dx()) / float64(target.Dy())
	sourceRatio := float64(bounds.Dx()) / float64(bounds.Dy())
	crop := bounds
	if sourceRatio > targetRatio {
		width := int(float64(bounds.Dy()) * targetRatio)
		offset := (bounds.Dx() - width) / 2
		crop = image.Rect(bounds.Min.X+offset, bounds.Min.Y, bounds.Min.X+offset+width, bounds.Max.Y)
	} else if sourceRatio < targetRatio {
		height := int(float64(bounds.Dx()) / targetRatio)
		offset := (bounds.Dy() - height) / 2
		crop = image.Rect(bounds.Min.X, bounds.Min.Y+offset, bounds.Max.X, bounds.Min.Y+offset+height)
	}
	xdraw.CatmullRom.Scale(canvas, target, source, crop, xdraw.Src, nil)
}

func drawContain(canvas *image.RGBA, target image.Rectangle, source image.Image) {
	bounds := source.Bounds()
	scale := math.Min(float64(target.Dx())/float64(bounds.Dx()), float64(target.Dy())/float64(bounds.Dy()))
	width := int(math.Round(float64(bounds.Dx()) * scale))
	height := int(math.Round(float64(bounds.Dy()) * scale))
	left := target.Min.X + (target.Dx()-width)/2
	top := target.Min.Y + (target.Dy()-height)/2
	xdraw.CatmullRom.Scale(canvas, image.Rect(left, top, left+width, top+height), source, bounds, xdraw.Src, nil)
}

func encodeCollage(canvas image.Image) ([]byte, error) {
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, canvas, &jpeg.Options{Quality: listingCollageJPEGQuality}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func BuildListingCaption(report evaluation.ReportView, hint string) string {
	head := listingCaptionHead(report)
	failed := listingCaptionFailedLines(report)
	hint = strings.TrimSpace(hint)
	for criteriaCount := len(report.Criteria); criteriaCount >= 0; criteriaCount-- {
		caption := assembleListingCaption(head, failed, listingCaptionCriteria(report.Criteria[:criteriaCount], len(report.Criteria)-criteriaCount), hint)
		if captionUnits(caption) <= maxListingCaptionUnits {
			return caption
		}
	}
	return assembleListingCaption(head, nil, "", hint)
}

func listingCaptionHead(report evaluation.ReportView) string {
	var facts []string
	if report.Price > 0 {
		facts = append(facts, fmt.Sprintf("<b>$%d / мес</b>", report.Price))
	} else {
		facts = append(facts, "<b>Цена не указана</b>")
	}
	if report.AreaM2 > 0 {
		facts = append(facts, formatArea(report.AreaM2)+" м²")
	}
	verdict := html.EscapeString(verdictLabel(report))
	if report.HasScore {
		verdict = fmt.Sprintf("%.0f/100 · %s", report.ScoreTotal, verdict)
	}
	facts = append(facts, verdict)
	lines := []string{strings.Join(facts, " · ")}
	if title := withoutHashtags(report.Title); title != "" {
		lines = append(lines, html.EscapeString(truncateRunes(title, maxListingCaptionTitleRunes)))
	}
	if report.FailureReason != "" {
		lines = append(lines, "⚠️ "+html.EscapeString(truncateRunes(report.FailureReason, maxListingCaptionDetailRunes)))
	}
	return strings.Join(lines, "\n")
}

func listingCaptionFailedLines(report evaluation.ReportView) []string {
	var lines []string
	for _, criterion := range report.FailedCriteria() {
		if len(lines) == maxListingCaptionFailedLines {
			break
		}
		line := "✗ " + html.EscapeString(criterion.Label)
		if criterion.Detail != "" {
			line += ": " + html.EscapeString(truncateRunes(criterion.Detail, maxListingCaptionDetailRunes))
		}
		lines = append(lines, line)
	}
	return lines
}

func listingCaptionCriteria(criteria []evaluation.ReportCriterion, omitted int) string {
	if len(criteria) == 0 {
		return ""
	}
	lines := make([]string, 0, len(criteria)+1)
	for _, criterion := range criteria {
		line := criterionMark(criterion.Outcome) + " " + html.EscapeString(criterion.Label)
		if criterion.Detail != "" {
			line += " — " + html.EscapeString(truncateRunes(criterion.Detail, maxListingCaptionDetailRunes))
		}
		lines = append(lines, line)
	}
	if omitted > 0 {
		lines = append(lines, fmt.Sprintf("и еще %d", omitted))
	}
	return "<blockquote expandable>Критерии\n" + strings.Join(lines, "\n") + "</blockquote>"
}

func assembleListingCaption(head string, failed []string, criteria string, hint string) string {
	parts := []string{head}
	if len(failed) > 0 {
		parts = append(parts, strings.Join(failed, "\n"))
	}
	if criteria != "" {
		parts = append(parts, criteria)
	}
	if hint != "" {
		parts = append(parts, "<i>"+html.EscapeString(hint)+"</i>")
	}
	return strings.Join(parts, "\n\n")
}

func captionUnits(text string) int {
	return len(utf16.Encode([]rune(text)))
}

func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func redactBotToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "<token>")
}

func withoutHashtags(text string) string {
	var words []string
	for _, word := range strings.Fields(text) {
		if !strings.HasPrefix(word, "#") {
			words = append(words, word)
		}
	}
	return strings.Join(words, " ")
}
