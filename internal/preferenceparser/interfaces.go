package preferenceparser

import (
	"context"
	"strings"

	"apartment-hunter/internal/cities"
	"apartment-hunter/internal/domain"
)

type PreferencesParser interface {
	Parse(ctx context.Context, input string) (*domain.UserPreferences, error)
}

type RequirementsNormalizer interface {
	Normalize(ctx context.Context, input string) (string, error)
}

type StructuredPreferencesExtractor interface {
	Extract(ctx context.Context, normalizedInput string) (*domain.UserPreferences, error)
}

type GeoResolver interface {
	ResolvePlace(ctx context.Context, query string) (*domain.ResolvedPlace, error)
	ResolveStreet(ctx context.Context, query string) (*domain.ResolvedStreet, error)
}

type GeometryBuilder interface {
	BuildSearchGeometry(ctx context.Context, city cities.City, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error)
}

type Service struct {
	Normalizer RequirementsNormalizer
	Extractor  StructuredPreferencesExtractor
}

type Draft struct {
	RawQuery        string
	NormalizedQuery string
	Preferences     domain.UserPreferences
}

func (s Service) Parse(ctx context.Context, input string) (*domain.UserPreferences, error) {
	draft, err := s.BuildDraft(ctx, input)
	if err != nil {
		return nil, err
	}
	return &draft.Preferences, nil
}

func (s Service) BuildDraft(ctx context.Context, input string) (Draft, error) {
	normalized, err := s.Normalizer.Normalize(ctx, input)
	if err != nil {
		return Draft{}, err
	}
	prefs, err := s.Extractor.Extract(ctx, normalized)
	if err != nil {
		return Draft{}, err
	}
	return Draft{
		RawQuery:        input,
		NormalizedQuery: normalized,
		Preferences:     NormalizePreferences(*prefs),
	}, nil
}

type NaturalLanguagePreferencesParser struct {
	Service
}

func NewNaturalLanguagePreferencesParser(normalizer RequirementsNormalizer, extractor StructuredPreferencesExtractor) NaturalLanguagePreferencesParser {
	return NaturalLanguagePreferencesParser{
		Service: Service{
			Normalizer: normalizer,
			Extractor:  extractor,
		},
	}
}

func NormalizePreferences(prefs domain.UserPreferences) domain.UserPreferences {
	if prefs.BudgetMin != nil {
		prefs.MinBudget = *prefs.BudgetMin
	} else if prefs.MinBudget > 0 {
		value := prefs.MinBudget
		prefs.BudgetMin = &value
	}
	if prefs.BudgetMax != nil {
		prefs.MaxBudget = *prefs.BudgetMax
	} else if prefs.MaxBudget > 0 {
		value := prefs.MaxBudget
		prefs.BudgetMax = &value
	}
	if prefs.AreaMin != nil {
		prefs.MinAreaM2 = *prefs.AreaMin
	} else if prefs.MinAreaM2 > 0 {
		value := prefs.MinAreaM2
		prefs.AreaMin = &value
	}
	for i, amenity := range prefs.RequiredAmenities {
		switch normalizeAmenitySlug(amenity) {
		case "dishwasher":
			prefs.RequiredAmenities[i] = "dishwasher"
			prefs.RequireDishwasher = true
		case "oven":
			prefs.RequiredAmenities[i] = "oven"
			prefs.RequireOven = true
		}
	}
	if prefs.RequireDishwasher && !containsString(prefs.RequiredAmenities, "dishwasher") {
		prefs.RequiredAmenities = append(prefs.RequiredAmenities, "dishwasher")
	}
	if prefs.RequireOven && !containsString(prefs.RequiredAmenities, "oven") {
		prefs.RequiredAmenities = append(prefs.RequiredAmenities, "oven")
	}
	if prefs.LocationPreferences == nil && prefs.Locations != nil {
		prefs.LocationPreferences = prefs.Locations
	}
	if prefs.Locations == nil && prefs.LocationPreferences != nil {
		prefs.Locations = prefs.LocationPreferences
	}
	if prefs.RequiredAmenities == nil {
		prefs.RequiredAmenities = []string{}
	}
	if prefs.PreferredAmenities == nil {
		prefs.PreferredAmenities = []string{}
	}
	if prefs.InteriorPreferences.Style == nil {
		prefs.InteriorPreferences.Style = []string{}
	}
	if prefs.InteriorPreferences.Reject == nil {
		prefs.InteriorPreferences.Reject = []string{}
	}
	if prefs.RejectRules == nil {
		prefs.RejectRules = []string{}
	}
	if prefs.LocationPreferences == nil {
		prefs.LocationPreferences = []domain.LocationPreference{}
	}
	if prefs.Locations == nil {
		prefs.Locations = []domain.LocationPreference{}
	}
	if prefs.UnresolvedLocationRules == nil {
		prefs.UnresolvedLocationRules = []string{}
	}
	if prefs.StopWords == nil {
		prefs.StopWords = []string{"юсдт", "usdt", "обмен", "обменяю", "обменять", "exchange"}
	}
	return prefs
}

func normalizeAmenitySlug(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("-", "_", " ", "_").Replace(value)
	switch value {
	case "dishwasher", "посудомойка", "посудомоечная_машина", "посудомоечная":
		return "dishwasher"
	case "oven", "духовка", "духовой_шкаф":
		return "oven"
	default:
		return value
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
