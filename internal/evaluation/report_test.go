package evaluation

import (
	"strings"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/scoring"
)

func TestBuildReportShowsCriteriaStatusesForRejectedListing(t *testing.T) {
	prefs := domain.UserPreferences{
		MaxBudget:         600,
		MinAreaM2:         40,
		RequireDishwasher: true,
	}
	listing := domain.ApartmentListing{
		Title:         "Apartment",
		Description:   "Apartment\n700 USD\n45 m2",
		Price:         700,
		AreaM2:        45,
		HasDishwasher: false,
	}
	reasons := scoring.BasicFilterReasons(listing, prefs)
	report := BuildReport(domain.ListingEvaluationRecord{
		Listing: listing,
		Status:  "rejected_basic",
		Score: &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  reasons,
			ScoreBreakdown:    scoring.BasicScoreBreakdown(listing, prefs),
		},
	}, prefs)

	for _, expected := range []string{
		"Статус: не прошло базовые фильтры",
		"Метраж: удовлетворено",
		"Цена: не удовлетворено",
		"Техника: пропущено",
		"Локация: пропущено",
		"Ремонт: пропущено",
		"Вид из окна: пропущено",
		"Причины отказа:",
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("expected report to contain %q:\n%s", expected, report)
		}
	}
}

func TestBuildReportDoesNotRejectAppliancesWhenBasicFilterSkippedPhotos(t *testing.T) {
	prefs := domain.UserPreferences{
		MaxBudget:         600,
		MinAreaM2:         50,
		RequireDishwasher: true,
		RequireOven:       true,
	}
	listing := domain.ApartmentListing{
		Title: "Telegram listing",
		Price: 700,
	}
	report := BuildReport(domain.ListingEvaluationRecord{
		Listing: listing,
		Status:  "rejected_basic",
		Score: &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  scoring.BasicFilterReasons(listing, prefs),
			ScoreBreakdown:    scoring.BasicScoreBreakdown(listing, prefs),
		},
	}, prefs)

	for _, expected := range []string{
		"Техника: пропущено (не анализировалась по фото из-за базового фильтра)",
		"цена 700 выше максимального бюджета 600",
		"метраж не найден",
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("expected report to contain %q:\n%s", expected, report)
		}
	}
	for _, unexpected := range []string{
		"Техника: не удовлетворено",
		"метраж 0.0 ниже минимума 50.0",
		"посудомойка обязательна",
		"духовка обязательна",
		"Техника: 0/10",
	} {
		if strings.Contains(report, unexpected) {
			t.Fatalf("expected report not to contain %q:\n%s", unexpected, report)
		}
	}
}

func TestBuildReportLocalizesRejectionReasons(t *testing.T) {
	prefs := domain.UserPreferences{
		MaxBudget:         600,
		MinAreaM2:         50,
		RequireDishwasher: true,
		RequireOven:       true,
	}
	listing := domain.ApartmentListing{
		Title: "Telegram listing",
	}
	report := BuildReport(domain.ListingEvaluationRecord{
		Listing: listing,
		Status:  "rejected_ai",
		Score: &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons: []string{
				"area 0.0 is below minimum 50.0",
				"dishwasher is required",
				"oven is required",
			},
			ScoreBreakdown: scoring.BasicScoreBreakdown(listing, prefs),
		},
	}, prefs)

	for _, expected := range []string{
		"метраж 0.0 ниже минимума 50.0",
		"посудомойка обязательна",
		"духовка обязательна",
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("expected report to contain %q:\n%s", expected, report)
		}
	}
	for _, unexpected := range []string{
		"area 0.0 is below minimum 50.0",
		"dishwasher is required",
		"oven is required",
	} {
		if strings.Contains(report, unexpected) {
			t.Fatalf("expected report not to contain %q:\n%s", unexpected, report)
		}
	}
}

func TestBuildReportUsesEmojiForChecklistDecisions(t *testing.T) {
	prefs := domain.UserPreferences{
		MaxBudget:         600,
		MinAreaM2:         50,
		RequireDishwasher: true,
		RequireOven:       true,
	}
	report := BuildReport(domain.ListingEvaluationRecord{
		Listing: domain.ApartmentListing{
			Title:         "Apartment",
			Price:         650,
			AreaM2:        55,
			HasDishwasher: true,
			HasOven:       true,
		},
		Status: "rejected_basic",
		Score: &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  []string{"price 650 exceeds max budget 600"},
			ScoreBreakdown:    map[string]float64{"price": 0, "area": 1, "appliances": 1},
		},
	}, prefs)

	for _, expected := range []string{
		"✅ Метраж: удовлетворено",
		"❌ Цена: не удовлетворено",
		"✅ Техника: удовлетворено",
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("expected report to contain %q:\n%s", expected, report)
		}
	}
}
