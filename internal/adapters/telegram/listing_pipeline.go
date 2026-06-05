package telegram

import (
	"context"

	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/scoring"
)

type DuplicateRecorder interface {
	SaveDuplicate(ctx context.Context, listingID string, candidate dedup.DuplicateCandidate) error
}

type ListingProcessor struct {
	Deduplicator      dedup.ListingDeduplicator
	DuplicateRecorder DuplicateRecorder
	VisionEvaluator   scoring.InsightsEvaluator
}

type ListingProcessingResult struct {
	Duplicate *dedup.DuplicateCandidate
	Insights  *domain.ListingInsights
}

func (p ListingProcessor) ProcessListing(ctx context.Context, listing domain.ApartmentListing) (ListingProcessingResult, error) {
	candidate, err := p.Deduplicator.CheckDuplicate(ctx, listing)
	if err != nil {
		return ListingProcessingResult{}, err
	}
	if candidate != nil {
		if p.DuplicateRecorder != nil {
			if err := p.DuplicateRecorder.SaveDuplicate(ctx, listing.ID, *candidate); err != nil {
				return ListingProcessingResult{}, err
			}
		}
		return ListingProcessingResult{Duplicate: candidate}, nil
	}

	if err := p.Deduplicator.SaveFingerprint(ctx, listing.ID, listing.Photos); err != nil {
		return ListingProcessingResult{}, err
	}
	if p.VisionEvaluator == nil {
		return ListingProcessingResult{}, nil
	}
	insights := p.VisionEvaluator.Analyze(listing)
	return ListingProcessingResult{Insights: &insights}, nil
}
