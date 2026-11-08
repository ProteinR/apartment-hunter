package evaluation

import (
	"testing"

	"apartment-hunter/internal/domain"
)

func TestBuildReportViewListsOnlyFailedCriteriaWithRejectionDetails(t *testing.T) {
	prefs := domain.UserPreferences{MaxBudget: 700, MinAreaM2: 50}
	view := BuildReportView(domain.ListingEvaluationRecord{
		Listing:  domain.ApartmentListing{Title: "2+1 Rustaveli", Price: 680, AreaM2: 61},
		Status:   "rejected_ai",
		Insights: &domain.ListingInsights{Style: "classic", ViewQuality: 0.2},
		Score: &domain.ListingScore{
			ScoreTotal:       41,
			RejectionReasons: []string{"glossy ceilings detected", "windows facing neighboring house detected"},
		},
		Source: domain.ListingSourceRef{ChannelUsername: "batumi_flats", MessageLink: "https://t.me/batumi_flats/9921"},
	}, prefs)

	failed := view.FailedCriteria()
	if len(failed) != 2 {
		t.Fatalf("expected 2 failed criteria, got %#v", failed)
	}
	if failed[0].Label != "Ремонт" || failed[0].Detail != "обнаружены глянцевые потолки, classic" {
		t.Fatalf("unexpected interior criterion: %#v", failed[0])
	}
	if failed[1].Label != "Вид из окна" || failed[1].Detail != "окна выходят на соседний дом, 2/10" {
		t.Fatalf("unexpected view criterion: %#v", failed[1])
	}
	if len(view.Criteria) != 6 {
		t.Fatalf("expected full criteria list of 6, got %d", len(view.Criteria))
	}
	if view.SourceLabel != "@batumi_flats" || view.Passed() {
		t.Fatalf("unexpected view: %#v", view)
	}
}

func TestBuildReportViewHasNoFailedCriteriaForPassedListing(t *testing.T) {
	prefs := domain.UserPreferences{MaxBudget: 700, MinAreaM2: 50}
	view := BuildReportView(domain.ListingEvaluationRecord{
		Listing:  domain.ApartmentListing{Title: "2+1", Price: 650, AreaM2: 58},
		Status:   "passed",
		Insights: &domain.ListingInsights{Style: "modern", ViewQuality: 0.8},
		Score:    &domain.ListingScore{ScoreTotal: 82, PassedHardFilters: true},
	}, prefs)
	if !view.Passed() || len(view.FailedCriteria()) != 0 {
		t.Fatalf("expected passed view without failures: %#v", view)
	}
}
