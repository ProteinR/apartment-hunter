package dedup

import (
	"context"
	"fmt"
	"math"

	"apartment-hunter/internal/domain"
)

type Service struct {
	hasher ImageHasher
	repo   ImageHashRepository
}

func NewService(hasher ImageHasher, repo ImageHashRepository) Service {
	return Service{hasher: hasher, repo: repo}
}

func (s Service) CheckDuplicate(ctx context.Context, listing domain.ApartmentListing) (*DuplicateCandidate, error) {
	fingerprints, err := s.fingerprintPhotos(ctx, listing.Photos)
	if err != nil {
		return nil, err
	}
	if len(fingerprints) == 0 {
		return nil, nil
	}

	stored, err := s.repo.ListImageFingerprints(ctx)
	if err != nil {
		return nil, err
	}

	aggregates := map[string]*matchAggregate{}
	for _, fingerprint := range fingerprints {
		best := bestListingMatch(fingerprint, stored)
		if best == nil {
			continue
		}
		aggregate := aggregates[best.listingID]
		if aggregate == nil {
			aggregate = &matchAggregate{listingID: best.listingID}
			aggregates[best.listingID] = aggregate
		}
		aggregate.matchedImages++
		if best.strong {
			aggregate.strongMatches++
		} else {
			aggregate.weakMatches++
		}
		aggregate.bestDistance = minDistance(aggregate.bestDistance, best.distance)
	}

	var best *DuplicateCandidate
	for _, aggregate := range aggregates {
		candidate := candidateFromAggregate(*aggregate)
		if candidate == nil {
			continue
		}
		if best == nil || candidate.Confidence > best.Confidence {
			best = candidate
		}
	}

	return best, nil
}

func (s Service) SaveFingerprint(ctx context.Context, listingID string, photos []domain.ApartmentPhoto) error {
	fingerprints, err := s.fingerprintPhotos(ctx, photos)
	if err != nil {
		return err
	}
	return s.repo.SaveImageFingerprints(ctx, listingID, fingerprints)
}

func (s Service) fingerprintPhotos(ctx context.Context, photos []domain.ApartmentPhoto) ([]ListingImageFingerprint, error) {
	fingerprints := make([]ListingImageFingerprint, 0, len(photos))
	for _, photo := range photos {
		if photo.URL == "" {
			continue
		}
		hashes, err := s.hasher.HashURL(ctx, photo.URL)
		if err != nil {
			return nil, fmt.Errorf("hash image %s: %w", photo.URL, err)
		}
		fingerprints = append(fingerprints, ListingImageFingerprint{
			ImageURL: photo.URL,
			Hashes:   hashes,
		})
	}
	return fingerprints, nil
}

type imageMatch struct {
	listingID string
	strong    bool
	distance  int
}

type matchAggregate struct {
	listingID     string
	matchedImages int
	strongMatches int
	weakMatches   int
	bestDistance  int
}

func bestListingMatch(fingerprint ListingImageFingerprint, stored []StoredListingImage) *imageMatch {
	var best *imageMatch
	for _, existing := range stored {
		strong, weak, distance := imageHashesMatch(fingerprint.Hashes, existing.Hashes)
		if !strong && !weak {
			continue
		}
		match := &imageMatch{listingID: existing.ListingID, strong: strong, distance: distance}
		if best == nil || betterMatch(match, best) {
			best = match
		}
	}
	return best
}

func betterMatch(candidate, current *imageMatch) bool {
	if candidate.strong != current.strong {
		return candidate.strong
	}
	return candidate.distance < current.distance
}

func imageHashesMatch(left, right ImageHashes) (bool, bool, int) {
	if left.SHA256 != "" && left.SHA256 == right.SHA256 {
		return true, false, 0
	}

	bestFull := minHashDistance([]string{left.PHashFull, left.DHashFull}, []string{right.PHashFull, right.DHashFull})
	if bestFull <= DefaultStrongPerceptualThreshold {
		return true, false, bestFull
	}
	if bestFull <= DefaultWeakPerceptualThreshold {
		return false, true, bestFull
	}

	bestCrop := minHashDistance(
		[]string{left.PHashFull, left.PHashCenter80, left.PHashCenter60},
		[]string{right.PHashFull, right.PHashCenter80, right.PHashCenter60},
	)
	if bestCrop <= DefaultStrongPerceptualThreshold {
		return true, false, bestCrop
	}
	if bestCrop <= DefaultCropCrossThreshold {
		return false, true, bestCrop
	}

	return false, false, math.MaxInt
}

func minHashDistance(leftHashes, rightHashes []string) int {
	best := math.MaxInt
	for _, left := range leftHashes {
		if left == "" {
			continue
		}
		for _, right := range rightHashes {
			if right == "" {
				continue
			}
			distance, err := HammingDistanceHex(left, right)
			if err == nil && distance < best {
				best = distance
			}
		}
	}
	return best
}

func candidateFromAggregate(aggregate matchAggregate) *DuplicateCandidate {
	switch {
	case aggregate.matchedImages >= 3:
		return &DuplicateCandidate{
			CanonicalListingID: aggregate.listingID,
			MatchedImages:      aggregate.matchedImages,
			StrongMatches:      aggregate.strongMatches,
			WeakMatches:        aggregate.weakMatches,
			Confidence:         0.95,
			Reason:             "3+ matched images",
		}
	case aggregate.strongMatches >= 2:
		return &DuplicateCandidate{
			CanonicalListingID: aggregate.listingID,
			MatchedImages:      aggregate.matchedImages,
			StrongMatches:      aggregate.strongMatches,
			WeakMatches:        aggregate.weakMatches,
			Confidence:         0.90,
			Reason:             "2 strong image matches",
		}
	case aggregate.strongMatches >= 1 && aggregate.weakMatches >= 2:
		return &DuplicateCandidate{
			CanonicalListingID: aggregate.listingID,
			MatchedImages:      aggregate.matchedImages,
			StrongMatches:      aggregate.strongMatches,
			WeakMatches:        aggregate.weakMatches,
			Confidence:         0.85,
			Reason:             "1 strong and 2 weak image matches",
		}
	default:
		return nil
	}
}

func minDistance(left, right int) int {
	if left == 0 {
		return right
	}
	if right < left {
		return right
	}
	return left
}
