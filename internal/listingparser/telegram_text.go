package listingparser

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/sources"
)

var (
	pricePattern = regexp.MustCompile(`(?i)(?:[$₾][ \t]*([0-9][0-9 \t.,]{0,8})|(?:usd|gel|lari)[ \t]*([0-9][0-9 \t.,]{0,8})|([0-9][0-9 \t.,]{0,8})[ \t]*(?:[$₾]|usd|gel|lari))`)
	areaPattern  = regexp.MustCompile(`(?i)([0-9]+(?:[.,][0-9]+)?)\s*(?:m2|m²|кв\.?\s?м|sqm)`)
)

func FromChannelMessage(message sources.ChannelMessage) domain.ApartmentListing {
	text := strings.TrimSpace(message.Text)
	title := firstNonEmptyLine(text)
	if title == "" {
		title = "Telegram listing"
	}

	photos := make([]domain.ApartmentPhoto, 0, len(message.Photos))
	for _, photo := range message.Photos {
		photos = append(photos, domain.ApartmentPhoto{
			URL:     photo.ID,
			Caption: photo.Caption,
		})
	}

	return domain.ApartmentListing{
		ID:          listingID(message),
		Title:       title,
		Description: text,
		Price:       parsePrice(text),
		AreaM2:      parseArea(text),
		Photos:      photos,
	}
}

func listingID(message sources.ChannelMessage) string {
	if message.ChannelID != "" && message.MessageID != 0 {
		return message.ChannelID + ":" + strconv.Itoa(message.MessageID)
	}
	hash := sha256.Sum256([]byte(message.Text))
	return "telegram:" + hex.EncodeToString(hash[:8])
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

func parsePrice(text string) int {
	match := pricePattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0
	}
	value := ""
	for _, candidate := range match[1:] {
		if strings.TrimSpace(candidate) != "" {
			value = candidate
			break
		}
	}
	if value == "" {
		return 0
	}
	value = strings.NewReplacer(" ", "", ",", "", ".", "").Replace(value)
	price, _ := strconv.Atoi(value)
	return price
}

func parseArea(text string) float64 {
	match := areaPattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0
	}
	value := strings.ReplaceAll(match[1], ",", ".")
	area, _ := strconv.ParseFloat(value, 64)
	return area
}
