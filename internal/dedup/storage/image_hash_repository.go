package storage

import (
	"context"
	"strconv"
	"sync"
	"time"

	"apartment-hunter/internal/dedup"
)

type InMemoryImageHashRepository struct {
	mu         sync.Mutex
	nextID     int
	images     []dedup.StoredListingImage
	duplicates []dedup.ListingDuplicateRecord
}

func NewInMemoryImageHashRepository() *InMemoryImageHashRepository {
	return &InMemoryImageHashRepository{}
}

func (r *InMemoryImageHashRepository) SaveImageFingerprints(ctx context.Context, listingID string, fingerprints []dedup.ListingImageFingerprint) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for _, fingerprint := range fingerprints {
		r.nextID++
		r.images = append(r.images, dedup.StoredListingImage{
			ID:             strconv.Itoa(r.nextID),
			ListingID:      listingID,
			SourceImageURL: fingerprint.ImageURL,
			Hashes:         fingerprint.Hashes,
			CreatedAt:      time.Now().UTC(),
		})
	}
	return nil
}

func (r *InMemoryImageHashRepository) ListImageFingerprints(ctx context.Context) ([]dedup.StoredListingImage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]dedup.StoredListingImage, len(r.images))
	copy(out, r.images)
	return out, nil
}

func (r *InMemoryImageHashRepository) SaveDuplicate(ctx context.Context, listingID string, candidate dedup.DuplicateCandidate) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.nextID++
	r.duplicates = append(r.duplicates, dedup.ListingDuplicateRecord{
		ID:                 strconv.Itoa(r.nextID),
		ListingID:          listingID,
		CanonicalListingID: candidate.CanonicalListingID,
		Confidence:         candidate.Confidence,
		Reason:             candidate.Reason,
		CreatedAt:          time.Now().UTC(),
	})
	return nil
}

func (r *InMemoryImageHashRepository) ListDuplicates(ctx context.Context) ([]dedup.ListingDuplicateRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]dedup.ListingDuplicateRecord, len(r.duplicates))
	copy(out, r.duplicates)
	return out, nil
}
