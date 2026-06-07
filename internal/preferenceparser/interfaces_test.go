package preferenceparser

import (
	"context"
	"testing"

	"apartment-hunter/internal/domain"
)

func TestServiceBuildDraftNormalizesThenExtracts(t *testing.T) {
	normalizer := recordingNormalizer{output: "Бюджет:\n* до 650 USD"}
	extractor := recordingExtractor{prefs: domain.UserPreferences{
		BudgetMax:         intPtr(650),
		RequiredAmenities: []string{"dishwasher"},
	}}
	service := Service{Normalizer: &normalizer, Extractor: &extractor}

	draft, err := service.BuildDraft(context.Background(), "ищу до 650 с посудомойкой")
	if err != nil {
		t.Fatal(err)
	}
	if normalizer.input != "ищу до 650 с посудомойкой" {
		t.Fatalf("normalizer got %q", normalizer.input)
	}
	if extractor.input != "Бюджет:\n* до 650 USD" {
		t.Fatalf("extractor got %q", extractor.input)
	}
	if draft.RawQuery != "ищу до 650 с посудомойкой" || draft.NormalizedQuery != "Бюджет:\n* до 650 USD" {
		t.Fatalf("unexpected draft %+v", draft)
	}
	if draft.Preferences.MaxBudget != 650 || !draft.Preferences.RequireDishwasher {
		t.Fatalf("preferences were not normalized for legacy scoring: %+v", draft.Preferences)
	}
}

type recordingNormalizer struct {
	input  string
	output string
}

func (n *recordingNormalizer) Normalize(ctx context.Context, input string) (string, error) {
	n.input = input
	return n.output, nil
}

type recordingExtractor struct {
	input string
	prefs domain.UserPreferences
}

func (e *recordingExtractor) Extract(ctx context.Context, input string) (*domain.UserPreferences, error) {
	e.input = input
	return &e.prefs, nil
}

func intPtr(value int) *int {
	return &value
}
