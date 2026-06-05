package telegram

import (
	"context"
	"testing"

	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"
)

type fakeDeduplicator struct {
	candidate       *dedup.DuplicateCandidate
	saveFingerprint bool
}

func (f *fakeDeduplicator) CheckDuplicate(ctx context.Context, listing domain.ApartmentListing) (*dedup.DuplicateCandidate, error) {
	return f.candidate, nil
}

func (f *fakeDeduplicator) SaveFingerprint(ctx context.Context, listingID string, photos []domain.ApartmentPhoto) error {
	f.saveFingerprint = true
	return nil
}

type fakeDuplicateRecorder struct {
	saved bool
}

func (f *fakeDuplicateRecorder) SaveDuplicate(ctx context.Context, listingID string, candidate dedup.DuplicateCandidate) error {
	f.saved = true
	return nil
}

type countingVisionEvaluator struct {
	calls int
}

func (e *countingVisionEvaluator) Analyze(listing domain.ApartmentListing) domain.ListingInsights {
	e.calls++
	return domain.ListingInsights{Style: "modern"}
}

func TestDuplicateListingSkipsVisionEvaluatorCall(t *testing.T) {
	deduplicator := &fakeDeduplicator{
		candidate: &dedup.DuplicateCandidate{
			CanonicalListingID: "canonical-1",
			Confidence:         0.90,
			Reason:             "2 strong image matches",
		},
	}
	recorder := &fakeDuplicateRecorder{}
	vision := &countingVisionEvaluator{}
	processor := ListingProcessor{
		Deduplicator:      deduplicator,
		DuplicateRecorder: recorder,
		VisionEvaluator:   vision,
	}

	result, err := processor.ProcessListing(context.Background(), domain.ApartmentListing{ID: "incoming"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicate == nil {
		t.Fatal("expected duplicate result")
	}
	if vision.calls != 0 {
		t.Fatalf("expected vision evaluator to be skipped, got %d calls", vision.calls)
	}
	if deduplicator.saveFingerprint {
		t.Fatal("expected duplicate listing not to be saved as canonical fingerprint")
	}
	if !recorder.saved {
		t.Fatal("expected duplicate alias to be saved")
	}
}

func TestNewListingSavesFingerprintAndCallsVisionEvaluator(t *testing.T) {
	deduplicator := &fakeDeduplicator{}
	vision := &countingVisionEvaluator{}
	processor := ListingProcessor{
		Deduplicator:    deduplicator,
		VisionEvaluator: vision,
	}

	result, err := processor.ProcessListing(context.Background(), domain.ApartmentListing{ID: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicate != nil {
		t.Fatalf("expected non-duplicate result, got %+v", result.Duplicate)
	}
	if !deduplicator.saveFingerprint {
		t.Fatal("expected new listing fingerprint to be saved")
	}
	if vision.calls != 1 {
		t.Fatalf("expected one vision call, got %d", vision.calls)
	}
}
