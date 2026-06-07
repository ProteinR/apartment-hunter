package preferenceparser

import (
	"context"

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
	BuildSearchGeometry(ctx context.Context, prefs []domain.LocationPreference) (*domain.GeoSearchArea, error)
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
		Preferences:     normalizePreferences(*prefs),
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

func normalizePreferences(prefs domain.UserPreferences) domain.UserPreferences {
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
	for _, amenity := range prefs.RequiredAmenities {
		switch amenity {
		case "dishwasher":
			prefs.RequireDishwasher = true
		case "oven":
			prefs.RequireOven = true
		}
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
	return prefs
}
