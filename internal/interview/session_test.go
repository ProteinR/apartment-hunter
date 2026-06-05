package interview

import "testing"

func TestSessionCollectsPreferences(t *testing.T) {
	session := NewSession()
	answers := []string{
		"400", "1200", "45", "да", "н", "1000", "y", "n", "Old Boulevard",
		"minimalist", "y", "n", "y", "n", "5", "4", "3", "2", "1",
	}

	for _, answer := range answers {
		_, _, err := session.Answer(answer)
		if err != nil {
			t.Fatalf("unexpected answer error: %v", err)
		}
	}

	if !session.Done() {
		t.Fatal("expected session to be complete")
	}
	prefs := session.Preferences()
	if prefs.MinBudget != 400 || prefs.MaxBudget != 1200 || prefs.MinAreaM2 != 45 {
		t.Fatalf("unexpected budget/area: %+v", prefs)
	}
	if len(prefs.Locations) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(prefs.Locations))
	}
	if !prefs.RequireDishwasher || prefs.RequireOven {
		t.Fatalf("unexpected appliances: %+v", prefs)
	}
}

func TestSessionSkipsCustomLocationsWithDot(t *testing.T) {
	session := NewSession()
	answers := []string{
		"400", "1200", "45", "y", "y", "1000", "n", "n", ".",
		"minimalist", "n", "n", "n", "n", "5", "4", "3", "2", "1",
	}

	for _, answer := range answers {
		_, _, err := session.Answer(answer)
		if err != nil {
			t.Fatalf("unexpected answer error: %v", err)
		}
	}

	if got := len(session.Preferences().Locations); got != 0 {
		t.Fatalf("expected no locations, got %d", got)
	}
}

func TestSessionAcceptsCoordinateLocation(t *testing.T) {
	session := NewSession()
	answers := []string{
		"400", "1200", "45", "y", "y", "1000", "n", "n", "41.6466, 41.6266; Old Boulevard",
		"minimalist", "n", "n", "n", "n", "5", "4", "3", "2", "1",
	}

	for _, answer := range answers {
		_, _, err := session.Answer(answer)
		if err != nil {
			t.Fatalf("unexpected answer error: %v", err)
		}
	}

	locations := session.Preferences().Locations
	if len(locations) != 2 {
		t.Fatalf("expected 2 locations, got %d", len(locations))
	}
	if locations[0].Latitude != 41.6466 || locations[0].Longitude != 41.6266 {
		t.Fatalf("unexpected coordinates %.6f %.6f", locations[0].Latitude, locations[0].Longitude)
	}
	if locations[1].Name != "Old Boulevard" {
		t.Fatalf("unexpected second location %q", locations[1].Name)
	}
}

func TestSessionRejectsInvalidInputWithoutAdvancing(t *testing.T) {
	session := NewSession()
	question, _, err := session.Answer("not a number")
	if err == nil {
		t.Fatal("expected invalid integer error")
	}
	if question.ID != "min_budget" {
		t.Fatalf("expected min_budget question, got %s", question.ID)
	}

	current, ok := session.CurrentQuestion()
	if !ok || current.ID != "min_budget" {
		t.Fatalf("expected session to stay on min_budget, got %+v", current)
	}
}
