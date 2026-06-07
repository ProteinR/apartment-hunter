package scoring

import (
	"testing"

	"apartment-hunter/internal/domain"
)

type fakeEvaluator struct {
	insights domain.ListingInsights
}

func (f fakeEvaluator) Analyze(domain.ApartmentListing) domain.ListingInsights {
	return f.insights
}

func TestScoreRejectsHardFilters(t *testing.T) {
	scorer := New(fakeEvaluator{insights: domain.ListingInsights{HasDarkWalls: true, InteriorQuality: 0.9, ViewQuality: 0.8}})
	result := scorer.Score(domain.ApartmentListing{
		Title:         "Too expensive",
		Price:         1800,
		AreaM2:        40,
		HasDishwasher: false,
		HasOven:       true,
	}, domain.UserPreferences{
		MaxBudget:         1200,
		MinAreaM2:         50,
		RequireDishwasher: true,
		Reject:            domain.RejectionPreferences{DarkWalls: true},
		Importance:        domain.ImportanceWeights{Location: 1, Interior: 1, View: 1, Price: 1, Appliances: 1},
	})

	if result.PassedHardFilters {
		t.Fatal("expected hard filters to fail")
	}
	if result.ScoreTotal != 0 {
		t.Fatalf("expected rejected listing score to be 0, got %v", result.ScoreTotal)
	}
	if len(result.RejectionReasons) != 4 {
		t.Fatalf("expected 4 rejection reasons, got %d: %v", len(result.RejectionReasons), result.RejectionReasons)
	}
}

func TestScoreUsesInsightAppliances(t *testing.T) {
	scorer := New(fakeEvaluator{insights: domain.ListingInsights{
		HasDishwasher:   true,
		HasOven:         true,
		InteriorQuality: 0.7,
		ViewQuality:     0.6,
	}})
	result := scorer.Score(domain.ApartmentListing{
		Title:  "Photo-confirmed kitchen",
		Price:  650,
		AreaM2: 55,
	}, domain.UserPreferences{
		MaxBudget:         800,
		MinAreaM2:         50,
		RequireDishwasher: true,
		RequireOven:       true,
		Importance:        domain.ImportanceWeights{Price: 1, Appliances: 1},
	})

	if !result.PassedHardFilters {
		t.Fatalf("expected listing to pass with appliances confirmed by insights, got %v", result.RejectionReasons)
	}
	if result.ScoreBreakdown["appliances"] != 1 {
		t.Fatalf("expected full appliance score, got %v", result.ScoreBreakdown["appliances"])
	}
}

func TestBasicFilterDoesNotRejectUnknownArea(t *testing.T) {
	reasons := BasicFilterReasons(domain.ApartmentListing{
		Title:  "Photo-only album item",
		Price:  500,
		AreaM2: 0,
	}, domain.UserPreferences{
		MaxBudget: 800,
		MinAreaM2: 50,
	})

	if len(reasons) != 0 {
		t.Fatalf("expected unknown area not to fail basic filters, got %v", reasons)
	}
}

func TestBasicFilterRejectsKnownAreaBelowMinimum(t *testing.T) {
	reasons := BasicFilterReasons(domain.ApartmentListing{
		Title:  "Too small",
		Price:  500,
		AreaM2: 40,
	}, domain.UserPreferences{
		MaxBudget: 800,
		MinAreaM2: 50,
	})

	if len(reasons) != 1 || reasons[0] != "area 40.0 is below minimum 50.0" {
		t.Fatalf("expected known undersized area rejection, got %v", reasons)
	}
}

func TestScoreWeightsBreakdown(t *testing.T) {
	scorer := New(fakeEvaluator{insights: domain.ListingInsights{Style: "minimalist", InteriorQuality: 0.8, ViewQuality: 0.6}})
	result := scorer.Score(domain.ApartmentListing{
		Title:         "Good fit",
		Price:         800,
		AreaM2:        55,
		District:      "6 May Park",
		HasDishwasher: true,
		HasOven:       true,
	}, domain.UserPreferences{
		MaxBudget:         1000,
		MinAreaM2:         45,
		RequireDishwasher: true,
		RequireOven:       true,
		DesiredStyle:      "minimalist",
		Locations:         []domain.LocationPreference{{Name: "6 May Park", MaxDistanceMeters: 1000}},
		Importance:        domain.ImportanceWeights{Location: 5, Interior: 4, View: 2, Price: 3, Appliances: 5},
	})

	if !result.PassedHardFilters {
		t.Fatalf("expected listing to pass, got reasons: %v", result.RejectionReasons)
	}
	if result.ScoreTotal <= 80 {
		t.Fatalf("expected strong score above 80, got %v", result.ScoreTotal)
	}
	if result.ScoreBreakdown["location"] != 1 {
		t.Fatalf("expected exact district location score 1, got %v", result.ScoreBreakdown["location"])
	}
}

func TestLocationScoreUsesDistance(t *testing.T) {
	score := locationScore(domain.ApartmentListing{Latitude: 41.6466, Longitude: 41.6266}, domain.UserPreferences{
		Locations: []domain.LocationPreference{{Name: "6 May Park", Latitude: 41.6466, Longitude: 41.6266, MaxDistanceMeters: 500}},
	})
	if score != 1 {
		t.Fatalf("expected colocated listing to score 1, got %v", score)
	}
}

func TestPriceScoreUsesPreferredRange(t *testing.T) {
	if got := priceScore(400, 400, 600); got != 1 {
		t.Fatalf("expected lower range boundary to score 1, got %v", got)
	}
	if got := priceScore(500, 400, 600); got != 0.8 {
		t.Fatalf("expected middle of range to score 0.8, got %v", got)
	}
	if got := priceScore(600, 400, 600); got != 0.6 {
		t.Fatalf("expected upper range boundary to score 0.6, got %v", got)
	}
	if got := priceScore(700, 400, 600); got != 0 {
		t.Fatalf("expected price above max budget to score 0, got %v", got)
	}
}
