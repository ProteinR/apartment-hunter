package evaluation

import (
	"fmt"
	"math"
	"strings"

	"apartment-hunter/internal/domain"
)

func BuildReport(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) string {
	lines := []string{
		"🏠 Отчет по объявлению",
		"📌 " + record.Listing.Title,
		"🚦 Статус: " + statusLabel(record.Status),
	}
	if record.Score != nil {
		lines = append(lines, fmt.Sprintf("🎯 Итог: %.0f/100", record.Score.ScoreTotal))
	}
	if record.Source.MessageLink != "" {
		lines = append(lines, "🔗 Сообщение: "+record.Source.MessageLink)
	} else if record.Source.ChannelID != "" && record.Source.MessageID != 0 {
		lines = append(lines, fmt.Sprintf("🔗 Источник: %s/%d", record.Source.ChannelID, record.Source.MessageID))
	}
	if text := originalMessageText(record.Listing.Description); text != "" {
		lines = append(lines, "", "📝 Текст объявления:", text)
	}

	lines = append(lines, "", "✅ Критерии:")
	for _, item := range checklistItems(record, prefs) {
		lines = append(lines, fmt.Sprintf("- %s %s: %s%s", item.Mark, item.Label, checklistStatus(item.Mark), item.Note))
	}

	if items := reportItems(record, prefs); len(items) > 0 {
		lines = append(lines, "", "📊 Баллы:")
		for _, item := range items {
			lines = append(lines, fmt.Sprintf("- %s: %d/10%s", item.Label, item.Score, item.Note))
		}
	}

	if record.Score != nil && len(record.Score.RejectionReasons) > 0 {
		lines = append(lines, "", "⛔ Причины отказа:")
		for _, reason := range record.Score.RejectionReasons {
			lines = append(lines, "- "+rejectionReasonLabel(reason))
		}
	}
	if record.FailureReason != "" {
		lines = append(lines, "", "⚠️ Ошибка: "+record.FailureReason)
	}
	if record.AIUsage != nil && record.AIUsage.TotalTokens > 0 {
		lines = append(lines, "", fmt.Sprintf("🤖 OpenAI: %s, tokens %d, cost $%.6f", record.AIUsage.Model, record.AIUsage.TotalTokens, record.AIUsage.CostUSD))
	}
	return strings.Join(lines, "\n")
}

type reportItem struct {
	Label string
	Score int
	Note  string
}

type checklistItem struct {
	Label string
	Mark  string
	Note  string
}

func checklistItems(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) []checklistItem {
	return []checklistItem{
		areaChecklistItem(record, prefs),
		priceChecklistItem(record, prefs),
		locationChecklistItem(record, prefs),
		appliancesChecklistItem(record, prefs),
		interiorChecklistItem(record),
		viewChecklistItem(record),
	}
}

func areaChecklistItem(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) checklistItem {
	note := areaNote(record.Listing, prefs)
	if prefs.MinAreaM2 <= 0 {
		return checklistItem{Label: "Метраж", Mark: "-", Note: " (требование не задано)"}
	}
	if record.Listing.AreaM2 <= 0 {
		return checklistItem{Label: "Метраж", Mark: "❌", Note: " (метраж не найден)"}
	}
	return checklistItem{Label: "Метраж", Mark: passMark(record.Listing.AreaM2 >= prefs.MinAreaM2), Note: note}
}

func priceChecklistItem(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) checklistItem {
	note := priceNote(record.Listing, prefs)
	if prefs.MinBudget <= 0 && prefs.MaxBudget <= 0 {
		return checklistItem{Label: "Цена", Mark: "-", Note: " (требование не задано)"}
	}
	if record.Listing.Price <= 0 {
		return checklistItem{Label: "Цена", Mark: "❌", Note: " (цена не найдена)"}
	}
	if prefs.MaxBudget > 0 && record.Listing.Price > prefs.MaxBudget {
		return checklistItem{Label: "Цена", Mark: "❌", Note: note}
	}
	return checklistItem{Label: "Цена", Mark: "✅", Note: note}
}

func locationChecklistItem(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) checklistItem {
	if len(prefs.Locations) == 0 && len(prefs.LocationPreferences) == 0 {
		return checklistItem{Label: "Локация", Mark: "-", Note: " (требование не задано)"}
	}
	if record.Status == "rejected_basic" {
		return checklistItem{Label: "Локация", Mark: "-", Note: " (не анализировалась из-за базового фильтра)"}
	}
	if record.Status == "rejected_location" {
		return checklistItem{Label: "Локация", Mark: "❌", Note: locationNote(record.Listing, prefs)}
	}
	if record.Status == "received" || record.Status == "failed" {
		return checklistItem{Label: "Локация", Mark: "-", Note: " (не завершена проверка)"}
	}
	return checklistItem{Label: "Локация", Mark: "✅", Note: locationNote(record.Listing, prefs)}
}

func appliancesChecklistItem(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) checklistItem {
	note := appliancesNote(record.Listing, prefs)
	if !prefs.RequireDishwasher && !prefs.RequireOven {
		return checklistItem{Label: "Техника", Mark: "-", Note: " (требование не задано)"}
	}
	ok := (!prefs.RequireDishwasher || record.Listing.HasDishwasher) &&
		(!prefs.RequireOven || record.Listing.HasOven)
	if record.Status == "rejected_basic" && !ok {
		return checklistItem{Label: "Техника", Mark: "-", Note: " (не анализировалась по фото из-за базового фильтра)"}
	}
	return checklistItem{Label: "Техника", Mark: passMark(ok), Note: note}
}

func interiorChecklistItem(record domain.ListingEvaluationRecord) checklistItem {
	if record.Insights == nil {
		return checklistItem{Label: "Ремонт", Mark: "-", Note: " (не анализировался)"}
	}
	ok := true
	if record.Score != nil {
		for _, reason := range record.Score.RejectionReasons {
			reason = strings.ToLower(reason)
			if strings.Contains(reason, "glossy ceilings") ||
				strings.Contains(reason, "dark walls") ||
				strings.Contains(reason, "old renovation") {
				ok = false
				break
			}
		}
	}
	note := ""
	if record.Insights.Style != "" && record.Insights.Style != "unknown" {
		note = " (" + record.Insights.Style + ")"
	}
	return checklistItem{Label: "Ремонт", Mark: passMark(ok), Note: note}
}

func viewChecklistItem(record domain.ListingEvaluationRecord) checklistItem {
	if record.Insights == nil {
		return checklistItem{Label: "Вид из окна", Mark: "-", Note: " (не анализировался)"}
	}
	ok := true
	if record.Score != nil {
		for _, reason := range record.Score.RejectionReasons {
			if strings.Contains(strings.ToLower(reason), "windows facing") {
				ok = false
				break
			}
		}
	}
	return checklistItem{Label: "Вид из окна", Mark: passMark(ok), Note: fmt.Sprintf(" (%d/10)", tenPoint(record.Insights.ViewQuality))}
}

func passMark(ok bool) string {
	if ok {
		return "✅"
	}
	return "❌"
}

func checklistStatus(mark string) string {
	switch mark {
	case "✅":
		return "удовлетворено"
	case "❌":
		return "не удовлетворено"
	default:
		return "пропущено"
	}
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
	if shouldShowAppliancesScore(record, prefs) {
		add("appliances", "Техника", appliancesNote(record.Listing, prefs))
	}
	return items
}

func shouldShowAppliancesScore(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) bool {
	if record.Status != "rejected_basic" {
		return true
	}
	return (!prefs.RequireDishwasher || record.Listing.HasDishwasher) &&
		(!prefs.RequireOven || record.Listing.HasOven)
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

func rejectionReasonLabel(reason string) string {
	var price, maxBudget int
	if _, err := fmt.Sscanf(reason, "price %d exceeds max budget %d", &price, &maxBudget); err == nil {
		return fmt.Sprintf("цена %d выше максимального бюджета %d", price, maxBudget)
	}

	var area, minArea float64
	if _, err := fmt.Sscanf(reason, "area %f is below minimum %f", &area, &minArea); err == nil {
		return fmt.Sprintf("метраж %.1f ниже минимума %.1f", area, minArea)
	}

	switch reason {
	case "dishwasher is required":
		return "посудомойка обязательна"
	case "oven is required":
		return "духовка обязательна"
	case "glossy ceilings detected":
		return "обнаружены глянцевые потолки"
	case "dark walls detected":
		return "обнаружены темные стены"
	case "old renovation detected":
		return "обнаружен старый ремонт"
	case "windows facing neighboring house detected":
		return "окна выходят на соседний дом"
	default:
		return reason
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

func originalMessageText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	const limit = 900
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit])) + "..."
}
