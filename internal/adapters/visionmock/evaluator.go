package visionmock

import (
	"strings"

	"apartment-hunter/internal/domain"
)

type Evaluator struct{}

func New() Evaluator {
	return Evaluator{}
}

func (Evaluator) Analyze(listing domain.ApartmentListing) domain.ListingInsights {
	text := strings.ToLower(listing.Title + " " + listing.Description)
	for _, photo := range listing.Photos {
		text += " " + strings.ToLower(photo.Caption+" "+photo.Room+" "+photo.URL)
	}

	insights := domain.ListingInsights{
		Style:           detectStyle(text),
		ViewQuality:     scoreByKeywords(text, []string{"sea view", "вид на море", "panoramic", "панорам"}, []string{"windows facing house", "окна в дом"}),
		InteriorQuality: scoreByKeywords(text, []string{"modern", "fresh", "designer", "новый ремонт", "соврем"}, []string{"old renovation", "старый ремонт", "dated"}),
	}

	insights.HasGlossyCeilings = containsAny(text, "glossy ceiling", "глянцевый потолок", "натяжной глянец")
	insights.HasDarkWalls = containsAny(text, "dark walls", "темные стены", "тёмные стены", "black walls")
	insights.HasOldRenovation = containsAny(text, "old renovation", "старый ремонт", "dated interior")
	insights.WindowsFacingHouse = containsAny(text, "windows facing house", "окна в соседний дом", "окна в дом")

	if containsAny(text, "dishwasher", "посудомойка", "посудомоечная") {
		insights.DescriptionHighlights = append(insights.DescriptionHighlights, "dishwasher mentioned")
	}
	if containsAny(text, "oven", "духовка", "духовой шкаф") {
		insights.DescriptionHighlights = append(insights.DescriptionHighlights, "oven mentioned")
	}

	return insights
}

func detectStyle(text string) string {
	switch {
	case containsAny(text, "minimalist", "минимализм", "minimal"):
		return "minimalist"
	case containsAny(text, "scandinavian", "сканди"):
		return "scandinavian"
	case containsAny(text, "loft", "лофт"):
		return "loft"
	case containsAny(text, "classic", "класс"):
		return "classic"
	case containsAny(text, "modern", "соврем"):
		return "modern"
	default:
		return "unknown"
	}
}

func scoreByKeywords(text string, positive, negative []string) float64 {
	score := 0.5
	for _, word := range positive {
		if strings.Contains(text, word) {
			score += 0.25
			break
		}
	}
	for _, word := range negative {
		if strings.Contains(text, word) {
			score -= 0.3
			break
		}
	}
	if score < 0 {
		return 0
	}
	if score > 1 {
		return 1
	}
	return score
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
