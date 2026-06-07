package cli

import (
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
)

func TestShouldSendTelegramNotificationHonorsDebugMode(t *testing.T) {
	enabled := true
	disabled := false

	if !shouldSendTelegramNotification(profiles.Profile{DebugMode: &enabled}, domain.ListingEvaluationRecord{Status: "rejected_basic"}) {
		t.Fatal("expected rejected listing to be sent when debug mode is enabled")
	}
	if shouldSendTelegramNotification(profiles.Profile{DebugMode: &disabled}, domain.ListingEvaluationRecord{Status: "rejected_basic"}) {
		t.Fatal("expected rejected listing to be skipped when debug mode is disabled")
	}
	if !shouldSendTelegramNotification(profiles.Profile{DebugMode: &disabled}, domain.ListingEvaluationRecord{Status: "passed"}) {
		t.Fatal("expected passed listing to be sent when debug mode is disabled")
	}
}
