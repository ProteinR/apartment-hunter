package openaipreferences

import (
	"strings"
	"testing"
)

func TestPreferencePromptsPreserveUserLanguage(t *testing.T) {
	for _, expected := range []string{
		"на том же языке",
		"Если запрос написан на русском",
		"не используй английские слова",
	} {
		if !strings.Contains(normalizationInstructions, expected) {
			t.Fatalf("normalization prompt should contain %q", expected)
		}
	}

	for _, expected := range []string{
		"Human-readable text fields must preserve the input language",
		"If the input is Russian",
		"unresolved_location_rules in the original input language",
	} {
		if !strings.Contains(extractionInstructions, expected) {
			t.Fatalf("extraction prompt should contain %q", expected)
		}
	}
}
