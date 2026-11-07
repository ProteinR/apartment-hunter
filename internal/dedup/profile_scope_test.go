package dedup

import (
	"context"
	"testing"
)

type profileImagesRepository struct {
	ImageHashRepository
	requestedProfileID string
}

func (r *profileImagesRepository) ListImageFingerprintsForProfile(ctx context.Context, profileID string) ([]StoredListingImage, error) {
	r.requestedProfileID = profileID
	return []StoredListingImage{{ListingID: profileID + "/batumi_rent:1"}}, nil
}

func TestProfileScopedRepositoryListsOnlyThatProfilesImages(t *testing.T) {
	repo := &profileImagesRepository{}

	images, err := ProfileScopedRepository(repo, "telegram_1").ListImageFingerprints(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if repo.requestedProfileID != "telegram_1" || len(images) != 1 {
		t.Fatalf("expected images of telegram_1 only, got %q %+v", repo.requestedProfileID, images)
	}
}
