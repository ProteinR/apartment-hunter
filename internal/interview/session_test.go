package interview

import "testing"

func TestSessionCollectsPreferences(t *testing.T) {
	session := NewSession()
	answers := []string{
		"400", "1200", "45", "y", "n", "1000", "y", "n", "Old Boulevard",
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
