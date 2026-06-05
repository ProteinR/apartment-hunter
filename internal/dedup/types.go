package dedup

import (
	"context"
	"time"

	"apartment-hunter/internal/domain"
)

const (
	DefaultStrongPerceptualThreshold = 6
	DefaultWeakPerceptualThreshold   = 10
	DefaultCropCrossThreshold        = 8
)

type ImageHashes struct {
	SHA256        string `json:"sha256"`
	PHashFull     string `json:"phash_full"`
	PHashCenter80 string `json:"phash_center_80"`
	PHashCenter60 string `json:"phash_center_60"`
	DHashFull     string `json:"dhash_full"`
}

type ListingImageFingerprint struct {
	ImageURL string      `json:"image_url"`
	Hashes   ImageHashes `json:"hashes"`
}

type DuplicateCandidate struct {
	CanonicalListingID string  `json:"canonical_listing_id"`
	MatchedImages      int     `json:"matched_images"`
	StrongMatches      int     `json:"strong_matches"`
	WeakMatches        int     `json:"weak_matches"`
	Confidence         float64 `json:"confidence"`
	Reason             string  `json:"reason"`
}

type StoredListingImage struct {
	ID             string
	ListingID      string
	Source         string
	SourceImageURL string
	Hashes         ImageHashes
	CreatedAt      time.Time
}

type ListingDuplicateRecord struct {
	ID                 string
	ListingID          string
	CanonicalListingID string
	Confidence         float64
	Reason             string
	CreatedAt          time.Time
}

type ImageHashRepository interface {
	SaveImageFingerprints(ctx context.Context, listingID string, fingerprints []ListingImageFingerprint) error
	ListImageFingerprints(ctx context.Context) ([]StoredListingImage, error)
	SaveDuplicate(ctx context.Context, listingID string, candidate DuplicateCandidate) error
	ListDuplicates(ctx context.Context) ([]ListingDuplicateRecord, error)
}

type ListingDeduplicator interface {
	CheckDuplicate(ctx context.Context, listing domain.ApartmentListing) (*DuplicateCandidate, error)
	SaveFingerprint(ctx context.Context, listingID string, photos []domain.ApartmentPhoto) error
}
