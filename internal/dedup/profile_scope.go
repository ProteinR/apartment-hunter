package dedup

import (
	"context"
	"strings"
)

type ProfileImageHashRepository interface {
	ImageHashRepository
	ListImageFingerprintsForProfile(ctx context.Context, profileID string) ([]StoredListingImage, error)
}

type profileScopedRepository struct {
	ProfileImageHashRepository
	profileID string
}

func ProfileScopedRepository(repo ProfileImageHashRepository, profileID string) ImageHashRepository {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return repo
	}
	return profileScopedRepository{ProfileImageHashRepository: repo, profileID: profileID}
}

func (r profileScopedRepository) ListImageFingerprints(ctx context.Context) ([]StoredListingImage, error) {
	return r.ProfileImageHashRepository.ListImageFingerprintsForProfile(ctx, r.profileID)
}
