package scoring

import (
	"fmt"
	"math"
	"strings"

	"apartment-hunter/internal/domain"
)

type InsightsEvaluator interface {
	Analyze(listing domain.ApartmentListing) domain.ListingInsights
}

type Scorer struct {
	evaluator InsightsEvaluator
}

func New(evaluator InsightsEvaluator) Scorer {
	return Scorer{evaluator: evaluator}
}

func (s Scorer) Score(listing domain.ApartmentListing, prefs domain.UserPreferences) domain.ListingScore {
	insights := s.evaluator.Analyze(listing)
	return ScoreWithInsights(listing, prefs, insights)
}

func ScoreWithInsights(listing domain.ApartmentListing, prefs domain.UserPreferences, insights domain.ListingInsights) domain.ListingScore {
	prefs = normalizeScoringPreferences(prefs)
	listing = listingWithInsightAppliances(listing, insights)
	reasons := hardFilterReasons(listing, prefs, insights)
	breakdown := BreakdownWithInsights(listing, prefs, insights)

	total := weightedTotal(breakdown, prefs.Importance)
	if len(reasons) > 0 {
		total = 0
	}

	return domain.ListingScore{
		PassedHardFilters: len(reasons) == 0,
		RejectionReasons:  reasons,
		ScoreTotal:        round2(total),
		ScoreBreakdown:    roundBreakdown(breakdown),
		ShortSummary:      summary(listing, insights, reasons, total),
	}
}

func BreakdownWithInsights(listing domain.ApartmentListing, prefs domain.UserPreferences, insights domain.ListingInsights) map[string]float64 {
	prefs = normalizeScoringPreferences(prefs)
	listing = listingWithInsightAppliances(listing, insights)
	return map[string]float64{
		"location":   locationScore(listing, prefs),
		"area":       areaScore(listing.AreaM2, prefs.MinAreaM2),
		"interior":   interiorScore(insights, prefs),
		"view":       clamp01(insights.ViewQuality),
		"price":      priceScore(listing.Price, prefs.MinBudget, prefs.MaxBudget),
		"appliances": appliancesScore(listing, prefs),
	}
}

func BasicScoreBreakdown(listing domain.ApartmentListing, prefs domain.UserPreferences) map[string]float64 {
	prefs = normalizeScoringPreferences(prefs)
	return map[string]float64{
		"location":   locationScore(listing, prefs),
		"area":       areaScore(listing.AreaM2, prefs.MinAreaM2),
		"price":      priceScore(listing.Price, prefs.MinBudget, prefs.MaxBudget),
		"appliances": appliancesScore(listing, prefs),
	}
}

func BasicFilterReasons(listing domain.ApartmentListing, prefs domain.UserPreferences) []string {
	prefs = normalizeScoringPreferences(prefs)
	reasons := []string{}
	if prefs.MaxBudget > 0 && listing.Price > prefs.MaxBudget {
		reasons = append(reasons, fmt.Sprintf("price %d exceeds max budget %d", listing.Price, prefs.MaxBudget))
	}
	if prefs.MinAreaM2 > 0 && listing.AreaM2 > 0 && listing.AreaM2 < prefs.MinAreaM2 {
		reasons = append(reasons, fmt.Sprintf("area %.1f is below minimum %.1f", listing.AreaM2, prefs.MinAreaM2))
	}
	return reasons
}

func normalizeScoringPreferences(prefs domain.UserPreferences) domain.UserPreferences {
	if prefs.BudgetMin != nil {
		prefs.MinBudget = *prefs.BudgetMin
	}
	if prefs.BudgetMax != nil {
		prefs.MaxBudget = *prefs.BudgetMax
	}
	if prefs.AreaMin != nil {
		prefs.MinAreaM2 = *prefs.AreaMin
	}
	for _, amenity := range prefs.RequiredAmenities {
		switch strings.ToLower(strings.TrimSpace(amenity)) {
		case "dishwasher":
			prefs.RequireDishwasher = true
		case "oven":
			prefs.RequireOven = true
		}
	}
	if prefs.Importance == (domain.ImportanceWeights{}) {
		prefs.Importance = domain.ImportanceWeights{
			Location:   prefs.Weights.Location,
			Interior:   prefs.Weights.Interior,
			View:       prefs.Weights.View,
			Price:      prefs.Weights.Price,
			Appliances: prefs.Weights.Amenities,
		}
		if prefs.Importance == (domain.ImportanceWeights{}) {
			prefs.Importance = domain.ImportanceWeights{
				Location:   1,
				Interior:   1,
				View:       1,
				Price:      1,
				Appliances: 1,
			}
		}
	}
	for _, reject := range prefs.InteriorPreferences.Reject {
		switch normalizeRuleSlug(reject) {
		case "glossyceiling", "glossyceilings":
			prefs.Reject.GlossyCeilings = true
		case "darkwalls", "darkwall":
			prefs.Reject.DarkWalls = true
		case "oldrenovation", "oldrepair":
			prefs.Reject.OldRenovation = true
		}
	}
	for _, rule := range prefs.RejectRules {
		switch normalizeRuleSlug(rule) {
		case "glossyceiling", "glossyceilings":
			prefs.Reject.GlossyCeilings = true
		case "darkwalls", "darkwall":
			prefs.Reject.DarkWalls = true
		case "oldrenovation", "oldrepair":
			prefs.Reject.OldRenovation = true
		case "windowsfacingneighborbuilding", "windowfacingneighborbuilding", "windowsfacingadjacentbuilding", "windowfacingadjacentbuilding", "windowsfacinghouse", "windowfacinghouse":
			prefs.Reject.WindowsFacingHouse = true
		}
	}
	return prefs
}

func normalizeRuleSlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("-", "", "_", "", " ", "").Replace(value)
	return value
}

func requiredApplianceReasons(listing domain.ApartmentListing, prefs domain.UserPreferences) []string {
	reasons := []string{}
	if prefs.RequireDishwasher && !listing.HasDishwasher {
		reasons = append(reasons, "dishwasher is required")
	}
	if prefs.RequireOven && !listing.HasOven {
		reasons = append(reasons, "oven is required")
	}
	return reasons
}

func hardFilterReasons(listing domain.ApartmentListing, prefs domain.UserPreferences, insights domain.ListingInsights) []string {
	listing = listingWithInsightAppliances(listing, insights)
	reasons := BasicFilterReasons(listing, prefs)
	reasons = append(reasons, requiredApplianceReasons(listing, prefs)...)
	if prefs.Reject.GlossyCeilings && insights.HasGlossyCeilings {
		reasons = append(reasons, "glossy ceilings detected")
	}
	if prefs.Reject.DarkWalls && insights.HasDarkWalls {
		reasons = append(reasons, "dark walls detected")
	}
	if prefs.Reject.OldRenovation && insights.HasOldRenovation {
		reasons = append(reasons, "old renovation detected")
	}
	if prefs.Reject.WindowsFacingHouse && insights.WindowsFacingHouse {
		reasons = append(reasons, "windows facing neighboring house detected")
	}
	return reasons
}

func listingWithInsightAppliances(listing domain.ApartmentListing, insights domain.ListingInsights) domain.ApartmentListing {
	if insights.HasDishwasher {
		listing.HasDishwasher = true
	}
	if insights.HasOven {
		listing.HasOven = true
	}
	return listing
}

func locationScore(listing domain.ApartmentListing, prefs domain.UserPreferences) float64 {
	if len(prefs.Locations) == 0 {
		return 0.5
	}

	best := 0.0
	for _, loc := range prefs.Locations {
		if strings.EqualFold(strings.TrimSpace(listing.District), strings.TrimSpace(loc.Name)) {
			best = math.Max(best, 1)
			continue
		}
		if loc.Latitude == 0 || loc.Longitude == 0 || listing.Latitude == 0 || listing.Longitude == 0 || loc.MaxDistanceMeters <= 0 {
			continue
		}
		distance := haversineMeters(listing.Latitude, listing.Longitude, loc.Latitude, loc.Longitude)
		score := 1 - (distance / float64(loc.MaxDistanceMeters))
		best = math.Max(best, clamp01(score))
	}
	return best
}

func interiorScore(insights domain.ListingInsights, prefs domain.UserPreferences) float64 {
	score := clamp01(insights.InteriorQuality)
	if prefs.DesiredStyle != "" && insights.Style != "unknown" {
		if strings.EqualFold(prefs.DesiredStyle, insights.Style) {
			score = math.Max(score, 1)
		} else {
			score *= 0.75
		}
	}
	return score
}

func priceScore(price, minBudget, maxBudget int) float64 {
	if price <= 0 {
		return 0.5
	}
	if maxBudget > 0 && price > maxBudget {
		return 0
	}
	if minBudget > 0 && maxBudget > 0 {
		if price <= minBudget {
			return 1
		}
		if maxBudget <= minBudget {
			return 1
		}
		const scoreAtMaxBudget = 0.6
		position := float64(price-minBudget) / float64(maxBudget-minBudget)
		return clamp01(1 - position*(1-scoreAtMaxBudget))
	}
	if maxBudget <= 0 {
		return 0.5
	}
	return clamp01(1 - float64(price)/float64(maxBudget)*0.55)
}

func areaScore(areaM2, minAreaM2 float64) float64 {
	if minAreaM2 <= 0 || areaM2 <= 0 {
		return 0.5
	}
	if areaM2 >= minAreaM2 {
		return 1
	}
	return clamp01(areaM2 / minAreaM2)
}

func appliancesScore(listing domain.ApartmentListing, prefs domain.UserPreferences) float64 {
	required := 0
	matched := 0
	if prefs.RequireDishwasher {
		required++
		if listing.HasDishwasher {
			matched++
		}
	}
	if prefs.RequireOven {
		required++
		if listing.HasOven {
			matched++
		}
	}
	if required == 0 {
		score := 0.5
		if listing.HasDishwasher {
			score += 0.25
		}
		if listing.HasOven {
			score += 0.25
		}
		return score
	}
	return float64(matched) / float64(required)
}

func weightedTotal(breakdown map[string]float64, weights domain.ImportanceWeights) float64 {
	weighted := []struct {
		name   string
		weight int
	}{
		{"location", weights.Location},
		{"interior", weights.Interior},
		{"view", weights.View},
		{"price", weights.Price},
		{"appliances", weights.Appliances},
	}

	totalWeight := 0
	sum := 0.0
	for _, item := range weighted {
		weight := item.weight
		if weight < 0 {
			weight = 0
		}
		totalWeight += weight
		sum += breakdown[item.name] * float64(weight)
	}
	if totalWeight == 0 {
		return 0
	}
	return sum / float64(totalWeight) * 100
}

func summary(listing domain.ApartmentListing, insights domain.ListingInsights, reasons []string, total float64) string {
	if len(reasons) > 0 {
		return fmt.Sprintf("%s rejected: %s.", listing.Title, strings.Join(reasons, "; "))
	}
	return fmt.Sprintf("%s passed hard filters with %.1f/100; style=%s.", listing.Title, total, insights.Style)
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadius = 6371000
	toRad := func(deg float64) float64 { return deg * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return earthRadius * 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func round2(value float64) float64 {
	return math.Round(value*100) / 100
}

func roundBreakdown(input map[string]float64) map[string]float64 {
	output := make(map[string]float64, len(input))
	for key, value := range input {
		output[key] = round2(value)
	}
	return output
}
