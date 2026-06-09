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
		"✅ Метраж (45.0 м2 при минимуме 40.0)",
		"❌ Цена (700 при лимите 600)",
		"➖ Локация (требование не задано)",
		"➖ Техника (не анализировалась по фото из-за базового фильтра)",
		"➖ Ремонт (не анализировался)",
		"➖ Вид из окна (не анализировался)",
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
		"➖ Техника (не анализировалась по фото из-за базового фильтра)",
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

func TestBuildReportLocalizesLocationRejectionReason(t *testing.T) {
	report := BuildReport(domain.ListingEvaluationRecord{
		Listing: domain.ApartmentListing{
			Title:     "Apartment",
			District:  "Batumi, Georgia",
			Latitude:  41.65110,
			Longitude: 41.63670,
		},
		Status: "rejected_location",
		Score: &domain.ListingScore{
			PassedHardFilters: false,
			RejectionReasons:  []string{"listing location is outside preferred search area"},
			ScoreBreakdown:    map[string]float64{"location": 0},
		},
	}, domain.UserPreferences{
		Locations: []domain.LocationPreference{{Name: "6 May Park"}},
	})

	for _, expected := range []string{
		"❌ Локация (вне зоны поиска)",
		"локация объявления вне выбранной зоны поиска",
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("expected report to contain %q:\n%s", expected, report)
		}
	}
	if strings.Contains(report, "listing location is outside preferred search area") {
		t.Fatalf("expected report not to contain untranslated location rejection reason:\n%s", report)
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
		"✅ Метраж (55.0 м2 при минимуме 50.0)",
		"❌ Цена (650 при лимите 600)",
		"✅ Техника (обязательная техника есть)",
	} {
		if !strings.Contains(report, expected) {
			t.Fatalf("expected report to contain %q:\n%s", expected, report)
		}
	}
}

func TestBuildReportSimplifiesLocationNote(t *testing.T) {
	prefs := domain.UserPreferences{
		Locations: []domain.LocationPreference{{Name: "Batumi"}},
	}

	// Case 1: Location check passed (status "passed" or "rejected_ai")
	reportPassed := BuildReport(domain.ListingEvaluationRecord{
		Listing: domain.ApartmentListing{
			Title:     "Apartment",
			District:  "Batumi",
			Latitude:  41.6,
			Longitude: 41.6,
		},
		Status: "passed",
	}, prefs)
	if !strings.Contains(reportPassed, "✅ Локация\n") && !strings.HasSuffix(reportPassed, "✅ Локация") {
		// Wait, it might be in the middle of lines, let's just check if it contains "✅ Локация" and does NOT contain coordinates
		if !strings.Contains(reportPassed, "✅ Локация") || strings.Contains(reportPassed, "✅ Локация (") {
			t.Fatalf("expected report to contain clean '✅ Локация' with no note, got:\n%s", reportPassed)
		}
	}

	// Case 2: Location unresolved
	reportUnresolved := BuildReport(domain.ListingEvaluationRecord{
		Listing: domain.ApartmentListing{
			Title: "Apartment",
		},
		Status: "rejected_location",
		Score: &domain.ListingScore{
			RejectionReasons: []string{"listing location is unresolved"},
		},
	}, prefs)
	if !strings.Contains(reportUnresolved, "❌ Локация (не определена)") {
		t.Fatalf("expected report to contain '❌ Локация (не определена)', got:\n%s", reportUnresolved)
	}
}

