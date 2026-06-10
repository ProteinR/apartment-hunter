package listingparser

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
	"apartment-hunter/internal/sources"
)

var (
	pricePattern = regexp.MustCompile(`(?i)(?:[$₾][ \t]*([0-9][0-9 \t.,]{0,8})|(?:usd|gel|lari)[ \t]*([0-9][0-9 \t.,]{0,8})|([0-9][0-9 \t.,]{0,8})[ \t]*(?:[$₾]|usd|gel|lari))`)
	areaPattern  = regexp.MustCompile(`(?i)([0-9]+(?:[.,][0-9]+)?)\s*(?:m2|m²|м2|м²|кв\.?\s?м\.?|м\.?\s?кв\.?|sqm|²)`)
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

	listing := domain.ApartmentListing{
		ID:          listingID(message),
		Title:       title,
		Description: text,
		Price:       parsePrice(text),
		AreaM2:      parseArea(text),
		Photos:      photos,
	}
	lowerText := strings.ToLower(text)
	listing.HasDishwasher = containsAny(lowerText, "dishwasher", "посудомойка", "посудомоечная", "посудомоечную")
	listing.HasOven = containsAny(lowerText, "oven", "духовка", "духовой шкаф", "духовым шкафом")
	if lat, lng, ok := geocoding.FindCoordinates(text); ok {
		listing.Latitude = lat
		listing.Longitude = lng
	}
	return listing
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
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
	matches := pricePattern.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return 0
	}

	var firstParsedPrice int
	hasFirstParsedPrice := false

	for _, m := range matches {
		var value string
		for i := 1; i < len(m)/2; i++ {
			startIdx := m[2*i]
			endIdx := m[2*i+1]
			if startIdx != -1 && endIdx != -1 {
				val := strings.TrimSpace(text[startIdx:endIdx])
				if val != "" {
					value = val
					break
				}
			}
		}
		if value == "" {
			continue
		}
		value = strings.NewReplacer(" ", "", ",", "", ".", "").Replace(value)
		price, err := strconv.Atoi(value)
		if err != nil || price <= 0 {
			continue
		}

		if !hasFirstParsedPrice {
			firstParsedPrice = price
			hasFirstParsedPrice = true
		}

		// Find the start of the line containing the match
		lineStart := 0
		for idx := m[0] - 1; idx >= 0; idx-- {
			if text[idx] == '\n' {
				lineStart = idx + 1
				break
			}
		}
		linePrefix := strings.ToLower(text[lineStart:m[0]])

		hasDepositKeyword := false
		for _, kw := range []string{"депозит", "залог", "deposit", "комиссия", "commission"} {
			idx := strings.LastIndex(linePrefix, kw)
			if idx != -1 {
				hasRentAfter := false
				suffix := linePrefix[idx+len(kw):]
				for _, rentKw := range []string{"цена", "price", "стоимость", "rent"} {
					if strings.Contains(suffix, rentKw) {
						hasRentAfter = true
						break
					}
				}
				if !hasRentAfter {
					hasDepositKeyword = true
					break
				}
			}
		}

		if !hasDepositKeyword {
			return price
		}
	}

	if hasFirstParsedPrice {
		return firstParsedPrice
	}
	return 0
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
