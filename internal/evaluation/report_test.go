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
		"Техника: не удовлетворено",
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
