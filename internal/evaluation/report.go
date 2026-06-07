package evaluation

import (
	"fmt"
	"math"
	"strings"

	"apartment-hunter/internal/domain"
)

func BuildReport(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) string {
	lines := []string{
		"Отчет по объявлению",
		record.Listing.Title,
		"Статус: " + statusLabel(record.Status),
	}
	if record.Score != nil {
		lines = append(lines, fmt.Sprintf("Итог: %.0f/100", record.Score.ScoreTotal))
	}
	if record.Source.MessageLink != "" {
		lines = append(lines, "Сообщение: "+record.Source.MessageLink)
	} else if record.Source.ChannelID != "" && record.Source.MessageID != 0 {
		lines = append(lines, fmt.Sprintf("Источник: %s/%d", record.Source.ChannelID, record.Source.MessageID))
	}

	lines = append(lines, "", "Баллы:")
	for _, item := range reportItems(record, prefs) {
		lines = append(lines, fmt.Sprintf("- %s: %d/10%s", item.Label, item.Score, item.Note))
	}

	if record.Score != nil && len(record.Score.RejectionReasons) > 0 {
		lines = append(lines, "", "Причины отказа:")
		for _, reason := range record.Score.RejectionReasons {
			lines = append(lines, "- "+reason)
		}
	}
	if record.FailureReason != "" {
		lines = append(lines, "", "Ошибка: "+record.FailureReason)
	}
	if record.AIUsage != nil && record.AIUsage.TotalTokens > 0 {
		lines = append(lines, "", fmt.Sprintf("OpenAI: %s, tokens %d, cost $%.6f", record.AIUsage.Model, record.AIUsage.TotalTokens, record.AIUsage.CostUSD))
	}
	return strings.Join(lines, "\n")
}

type reportItem struct {
	Label string
	Score int
	Note  string
}

func reportItems(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) []reportItem {
	if record.Score == nil {
		return nil
	}
	breakdown := record.Score.ScoreBreakdown
	items := []reportItem{}
	add := func(key, label, note string) {
		value, ok := breakdown[key]
		if !ok {
			return
		}
		items = append(items, reportItem{Label: label, Score: tenPoint(value), Note: note})
	}
	add("location", "Расположение", locationNote(record.Listing, prefs))
	add("area", "Метраж", areaNote(record.Listing, prefs))
	add("interior", "Ремонт", "")
	add("view", "Вид из окна", "")
	add("price", "Цена", priceNote(record.Listing, prefs))
	add("appliances", "Техника", appliancesNote(record.Listing, prefs))
	return items
}

func tenPoint(value float64) int {
	if value < 0 {
		value = 0
	}
	if value > 1 {
		value = 1
	}
	return int(math.Round(value * 10))
}

func statusLabel(status string) string {
	switch status {
	case "passed":
		return "прошло"
	case "rejected_basic":
		return "не прошло базовые фильтры"
	case "rejected_location":
		return "не прошло гео-фильтр"
	case "rejected_ai":
		return "не прошло AI-фильтры"
	case "duplicate":
		return "дубликат"
	case "failed":
		return "ошибка обработки"
	default:
		return status
	}
}

func priceNote(listing domain.ApartmentListing, prefs domain.UserPreferences) string {
	if listing.Price <= 0 {
		return " (цена не найдена)"
	}
	if prefs.MinBudget > 0 && prefs.MaxBudget > 0 {
		return fmt.Sprintf(" (%d, цель ближе к %d, максимум %d)", listing.Price, prefs.MinBudget, prefs.MaxBudget)
	}
	if prefs.MaxBudget > 0 {
		return fmt.Sprintf(" (%d при лимите %d)", listing.Price, prefs.MaxBudget)
	}
	if prefs.MinBudget > 0 {
		return fmt.Sprintf(" (%d при минимуме %d)", listing.Price, prefs.MinBudget)
	}
	return fmt.Sprintf(" (%d)", listing.Price)
}

func areaNote(listing domain.ApartmentListing, prefs domain.UserPreferences) string {
	if listing.AreaM2 <= 0 {
		return " (метраж не найден)"
	}
	if prefs.MinAreaM2 > 0 {
		return fmt.Sprintf(" (%.1f м2 при минимуме %.1f)", listing.AreaM2, prefs.MinAreaM2)
	}
	return fmt.Sprintf(" (%.1f м2)", listing.AreaM2)
}

func locationNote(listing domain.ApartmentListing, prefs domain.UserPreferences) string {
	if listing.District != "" {
		return " (" + listing.District + ")"
	}
	if len(prefs.Locations) == 0 {
		return " (локации не заданы)"
	}
	return ""
}

func appliancesNote(listing domain.ApartmentListing, prefs domain.UserPreferences) string {
	required := []string{}
	missing := []string{}
	if prefs.RequireDishwasher {
		required = append(required, "посудомойка")
		if !listing.HasDishwasher {
			missing = append(missing, "посудомойка")
		}
	}
	if prefs.RequireOven {
		required = append(required, "духовка")
		if !listing.HasOven {
			missing = append(missing, "духовка")
		}
	}
	if len(required) == 0 {
		return ""
	}
	if len(missing) == 0 {
		return " (обязательная техника есть)"
	}
	return " (нет: " + strings.Join(missing, ", ") + ")"
}
