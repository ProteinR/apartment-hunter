package telegram

import (
	"fmt"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"

	"github.com/go-telegram/bot/models"
)

func buildPreferencesPreview(draft preferenceparser.Draft) string {
	prefs := draft.Preferences
	var b strings.Builder
	fmt.Fprintf(&b, "Ваш запрос:\n\n%s\n\n", draft.RawQuery)
	fmt.Fprintf(&b, "Нормализованное описание:\n\n%s\n\n", draft.NormalizedQuery)
	b.WriteString("Распознанные фильтры:\n\n")
	if prefs.BudgetMin != nil || prefs.BudgetMax != nil {
		b.WriteString("Бюджет: ")
		switch {
		case prefs.BudgetMin != nil && prefs.BudgetMax != nil:
			fmt.Fprintf(&b, "от %d до %d USD\n", *prefs.BudgetMin, *prefs.BudgetMax)
		case prefs.BudgetMax != nil:
			fmt.Fprintf(&b, "до %d USD\n", *prefs.BudgetMax)
		case prefs.BudgetMin != nil:
			fmt.Fprintf(&b, "от %d USD\n", *prefs.BudgetMin)
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
	appendList(&b, "Обязательно", prefs.RequiredAmenities)
	appendList(&b, "Предпочтительно", append(prefs.PreferredAmenities, preferredLocations(prefs.LocationPreferences)...))
	appendList(&b, "Стиль", prefs.InteriorPreferences.Style)
	rejects := append([]string{}, prefs.InteriorPreferences.Reject...)
	rejects = append(rejects, prefs.RejectRules...)
	appendList(&b, "Исключить", rejects)
	appendLocations(&b, prefs.LocationPreferences)
	appendList(&b, "Неразобранные geo-правила", prefs.UnresolvedLocationRules)
	b.WriteString("\nСохранить?")
	return b.String()
}

func appendList(b *strings.Builder, title string, values []string) {
	values = compactStrings(values)
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(b, "%s:\n", title)
	for _, value := range values {
		fmt.Fprintf(b, "* %s\n", value)
	}
}

func appendLocations(b *strings.Builder, locations []domain.LocationPreference) {
	if len(locations) == 0 {
		return
	}
	b.WriteString("Районы:\n")
	for _, loc := range locations {
		name := loc.CenterName
		if strings.TrimSpace(name) == "" {
			name = loc.Name
		}
		if strings.TrimSpace(name) == "" {
			name = loc.RawText
		}
		if loc.RadiusMeters != nil {
			fmt.Fprintf(b, "* %s, радиус %d м\n", name, *loc.RadiusMeters)
		} else {
			fmt.Fprintf(b, "* %s\n", name)
		}
		if loc.BoundaryStreetName != nil && strings.TrimSpace(*loc.BoundaryStreetName) != "" {
			rule := ""
			if loc.BoundaryRule != nil {
				rule = *loc.BoundaryRule + " "
			}
			fmt.Fprintf(b, "* ограничение: %s%s\n", rule, *loc.BoundaryStreetName)
		}
	}
}

func preferredLocations(locations []domain.LocationPreference) []string {
	var out []string
	for _, loc := range locations {
		if loc.Type == "area" || loc.Type == "radius" || loc.Type == "radius_with_boundary" {
			name := loc.CenterName
			if strings.TrimSpace(name) == "" {
				name = loc.Name
			}
			if strings.TrimSpace(name) != "" {
				out = append(out, name)
			}
		}
	}
	return out
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

func confirmationKeyboard() models.ReplyMarkup {
	return &models.ReplyKeyboardMarkup{
		Keyboard: [][]models.KeyboardButton{{
			{Text: "Save"},
			{Text: "Edit"},
			{Text: "Cancel"},
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
