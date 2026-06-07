package telegram

import (
	"fmt"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"

	"github.com/go-telegram/bot/models"
)

const preferencesStartMessage = `Опиши, какую квартиру ищем, одним сообщением. Можно писать обычным текстом: бюджет, район, площадь, что обязательно, что желательно и что точно не подходит.

Например:
Ищу квартиру в аренду в Батуми на долгий срок. Бюджет 400-650$, площадь от 50 м2. Обязательно нужна посудомойка и духовка. Хочу свежий светлый ремонт в нейтральных или пастельных тонах, без глянцевых потолков. Важно, чтобы окна не смотрели вплотную в соседний дом. Район: около парка 6 мая в радиусе 1 км, но не дальше улицы Пушкина, либо Новый бульвар до 1 км от моря.`

func buildPreferencesPreview(draft preferenceparser.Draft) string {
	prefs := draft.Preferences
	lang := previewLanguage(draft.RawQuery + "\n" + draft.NormalizedQuery)
	var b strings.Builder
	fmt.Fprintf(&b, "Ваш запрос:\n\n%s\n\n", draft.RawQuery)
	fmt.Fprintf(&b, "Нормализованное описание:\n\n%s\n\n", displayFreeText(draft.NormalizedQuery, lang))
	b.WriteString("Распознанные фильтры:\n\n")
	if prefs.BudgetMin != nil || prefs.BudgetMax != nil {
		b.WriteString("Бюджет: ")
		switch {
		case prefs.BudgetMin != nil && prefs.BudgetMax != nil:
			fmt.Fprintf(&b, "от %d до %d %s\n", *prefs.BudgetMin, *prefs.BudgetMax, displayCurrency("USD", lang))
		case prefs.BudgetMax != nil:
			fmt.Fprintf(&b, "до %d %s\n", *prefs.BudgetMax, displayCurrency("USD", lang))
		case prefs.BudgetMin != nil:
			fmt.Fprintf(&b, "от %d %s\n", *prefs.BudgetMin, displayCurrency("USD", lang))
		}
	}
	if prefs.AreaMin != nil || prefs.AreaMax != nil {
		b.WriteString("Площадь: ")
		switch {
		case prefs.AreaMin != nil && prefs.AreaMax != nil:
			fmt.Fprintf(&b, "от %.0f до %.0f м2\n", *prefs.AreaMin, *prefs.AreaMax)
		case prefs.AreaMin != nil:
			fmt.Fprintf(&b, "от %.0f м2\n", *prefs.AreaMin)
		case prefs.AreaMax != nil:
			fmt.Fprintf(&b, "до %.0f м2\n", *prefs.AreaMax)
		}
	}
	appendList(&b, "Обязательно", prefs.RequiredAmenities, lang)
	appendList(&b, "Предпочтительно", withoutExisting(prefs.PreferredAmenities, prefs.RequiredAmenities), lang)
	appendList(&b, "Стиль", prefs.InteriorPreferences.Style, lang)
	rejects := append([]string{}, prefs.InteriorPreferences.Reject...)
	rejects = append(rejects, prefs.RejectRules...)
	appendList(&b, "Исключить", rejects, lang)
	appendLocations(&b, prefs.LocationPreferences, lang)
	appendList(&b, "Неразобранные гео-правила", prefs.UnresolvedLocationRules, lang)
	b.WriteString("\nСохранить?")
	return b.String()
}

func appendList(b *strings.Builder, title string, values []string, lang string) {
	values = compactStrings(values)
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", title)
	for _, value := range values {
		fmt.Fprintf(b, "* %s\n", displayPreferenceValue(value, lang))
	}
}

func appendLocations(b *strings.Builder, locations []domain.LocationPreference, lang string) {
	if len(locations) == 0 {
		return
	}
	b.WriteString("Районы:\n")
	for _, loc := range locations {
		name := loc.RawText
		if strings.TrimSpace(name) == "" {
			name = loc.CenterName
		}
		if strings.TrimSpace(name) == "" {
			name = loc.Name
		}
		if loc.RadiusMeters != nil {
			fmt.Fprintf(b, "* %s, радиус %d м\n", displayPreferenceValue(name, lang), *loc.RadiusMeters)
		} else {
			fmt.Fprintf(b, "* %s\n", displayPreferenceValue(name, lang))
		}
		if loc.BoundaryStreetName != nil && strings.TrimSpace(*loc.BoundaryStreetName) != "" {
			rule := ""
			if loc.BoundaryRule != nil {
				rule = displayPreferenceValue(*loc.BoundaryRule, lang) + " "
			}
			street := displayPreferenceValue(*loc.BoundaryStreetName, lang)
			if lang == "ru" && loc.BoundaryRule != nil && displayPreferenceValue(*loc.BoundaryRule, lang) == "не дальше" {
				street = displayBoundaryTarget(*loc.BoundaryStreetName)
			}
			fmt.Fprintf(b, "* ограничение: %s%s\n", rule, street)
		}
	}
}

func compactStrings(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func withoutExisting(values, existing []string) []string {
	seen := map[string]bool{}
	for _, value := range existing {
		seen[strings.TrimSpace(value)] = true
	}
	var out []string
	for _, value := range values {
		if seen[strings.TrimSpace(value)] {
			continue
		}
		out = append(out, value)
	}
	return out
}

func previewLanguage(text string) string {
	for _, r := range text {
		if r >= 'А' && r <= 'я' || r == 'Ё' || r == 'ё' {
			return "ru"
		}
	}
	return "en"
}

func displayFreeText(value, lang string) string {
	value = strings.TrimSpace(value)
	if lang != "ru" {
		return value
	}
	for from, to := range russianFreeTextReplacements {
		value = strings.ReplaceAll(value, from, to)
	}
	return value
}

func displayCurrency(value, lang string) string {
	if lang == "ru" && value == "USD" {
		return "$"
	}
	return value
}

func displayPreferenceValue(value, lang string) string {
	value = strings.TrimSpace(value)
	if lang != "ru" {
		return value
	}
	if translated, ok := russianPreferenceLabels[value]; ok {
		return translated
	}
	value = displayFreeText(value, lang)
	if translated, ok := russianPreferenceLabels[value]; ok {
		return translated
	}
	if strings.HasPrefix(value, "not beyond ") {
		return "не дальше " + displayBoundaryTarget(strings.TrimSpace(strings.TrimPrefix(value, "not beyond ")))
	}
	if strings.Contains(value, "_") {
		return displaySlug(value)
	}
	return value
}

func displayBoundaryTarget(value string) string {
	value = displayPreferenceValue(value, "ru")
	if strings.HasPrefix(value, "улица ") {
		return "улицы " + strings.TrimSpace(strings.TrimPrefix(value, "улица "))
	}
	return value
}

func displaySlug(value string) string {
	parts := strings.Split(value, "_")
	translated := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			continue
		}
		if label, ok := russianSlugParts[part]; ok {
			translated = append(translated, label)
		}
	}
	if len(translated) == 0 {
		return "неуточненное требование"
	}
	return strings.Join(translated, " ")
}

var russianFreeTextReplacements = map[string]string{
	"6 May Park":       "парк 6 мая",
	"Pushkin Street":   "улица Пушкина",
	"New Boulevard":    "Новый бульвар",
	"USD":              "$",
	"fresh":            "свежий",
	"light renovation": "светлый ремонт",
}

var russianPreferenceLabels = map[string]string{
	"bright":                                 "светлый ремонт",
	"dark_old_renovation":                    "темный старый ремонт",
	"dark_walls":                             "темные стены",
	"dishwasher":                             "посудомойка",
	"fresh":                                  "свежий ремонт",
	"light_renovation":                       "светлый ремонт",
	"fresh_renovation":                       "свежий ремонт",
	"glossy_ceiling":                         "глянцевые потолки",
	"grey_tones":                             "серые тона",
	"neutral_tones":                          "нейтральные тона",
	"old_renovation":                         "старый ремонт",
	"oven":                                   "духовка",
	"pastel_tones":                           "пастельные тона",
	"not beyond":                             "не дальше",
	"not beyond Pushkin Street":              "не дальше улицы Пушкина",
	"radius":                                 "радиус",
	"sea":                                    "моря",
	"window_facing_adjacent_building":        "окна вплотную к соседнему дому",
	"window_facing_construction_site":        "окна на строящийся объект",
	"windows_facing_adjacent_building_close": "окна вплотную к соседнему дому",
	"windows_facing_house":                   "окна в соседний дом",
}

var russianSlugParts = map[string]string{
	"adjacent":     "соседний",
	"building":     "дом",
	"ceiling":      "потолок",
	"construction": "стройка",
	"dark":         "темный",
	"facing":       "вид",
	"fresh":        "свежий",
	"glossy":       "глянцевый",
	"light":        "светлый",
	"old":          "старый",
	"renovation":   "ремонт",
	"site":         "объект",
	"window":       "окно",
	"windows":      "окна",
}

func confirmationKeyboard() models.ReplyMarkup {
	return &models.ReplyKeyboardMarkup{
		Keyboard: [][]models.KeyboardButton{{
			{Text: "Сохранить"},
			{Text: "Изменить"},
			{Text: "Отмена"},
		}},
		ResizeKeyboard:  true,
		OneTimeKeyboard: true,
	}
}

func removeKeyboard() models.ReplyMarkup {
	return &models.ReplyKeyboardRemove{RemoveKeyboard: true}
}

func normalizeConfirmation(text string) string {
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "save", "сохранить":
		return "save"
	case "edit", "редактировать", "изменить":
		return "edit"
	case "cancel", "отмена", "отменить":
		return "cancel"
	default:
		return ""
	}
}
