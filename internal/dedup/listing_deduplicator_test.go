package dedup

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"

	"apartment-hunter/internal/domain"
)

func TestExactSHA256Duplicate(t *testing.T) {
	ctx := context.Background()
	repo := &testImageRepo{}
	service := NewService(NewImageHasher(), repo)
	imagePath := writePatternJPEG(t, "image-a.jpg", 128, 128, 95, patternA)

	err := service.SaveFingerprint(ctx, "canonical-1", []domain.ApartmentPhoto{{URL: imagePath}})
	if err != nil {
		t.Fatal(err)
	}

	candidate, err := service.CheckDuplicate(ctx, domain.ApartmentListing{
		ID:     "incoming",
		Photos: []domain.ApartmentPhoto{{URL: imagePath}, {URL: imagePath}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil {
		t.Fatal("expected duplicate candidate")
	}
	if candidate.StrongMatches < 2 || candidate.Confidence < 0.90 {
		t.Fatalf("expected strong exact duplicate, got %+v", candidate)
	}
}

func TestResizedCompressedImageRecognizedByPerceptualHash(t *testing.T) {
	hasher := NewImageHasher()
	original := writePatternJPEG(t, "original.jpg", 160, 160, 95, patternA)
	resized := writePatternJPEG(t, "resized-compressed.jpg", 80, 80, 45, patternA)

	left, err := hasher.HashURL(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	right, err := hasher.HashURL(context.Background(), resized)
	if err != nil {
		t.Fatal(err)
	}

	strong, weak, distance := imageHashesMatch(left, right)
	if !strong && !weak {
		t.Fatalf("expected perceptual match, distance=%d left=%+v right=%+v", distance, left, right)
	}
}

func TestCroppedImageRecognizedThroughCenterCropHashes(t *testing.T) {
	hasher := NewImageHasher()
	original := writePatternJPEG(t, "original-crop.jpg", 160, 160, 95, patternA)
	cropped := writeCenterCropJPEG(t, "center-crop.jpg", 160, 160, 0.60, 90, patternA)

	left, err := hasher.HashURL(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	right, err := hasher.HashURL(context.Background(), cropped)
	if err != nil {
		t.Fatal(err)
	}

	strong, weak, distance := imageHashesMatch(left, right)
	if !strong && !weak {
		t.Fatalf("expected crop/full perceptual match, distance=%d left=%+v right=%+v", distance, left, right)
	}
}

func TestSameListingWithTwoMatchingPhotosMarkedDuplicate(t *testing.T) {
	ctx := context.Background()
	repo := &testImageRepo{}
	service := NewService(NewImageHasher(), repo)

	photo1 := writePatternJPEG(t, "photo1.jpg", 160, 160, 95, patternA)
	photo2 := writePatternJPEG(t, "photo2.jpg", 160, 160, 95, patternB)
	if err := service.SaveFingerprint(ctx, "canonical-1", []domain.ApartmentPhoto{{URL: photo1}, {URL: photo2}}); err != nil {
		t.Fatal(err)
	}

	incoming1 := writePatternJPEG(t, "incoming1.jpg", 80, 80, 45, patternA)
	incoming2 := writePatternJPEG(t, "incoming2.jpg", 80, 80, 45, patternB)
	candidate, err := service.CheckDuplicate(ctx, domain.ApartmentListing{
		ID:     "incoming",
		Photos: []domain.ApartmentPhoto{{URL: incoming2}, {URL: incoming1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate == nil {
		t.Fatal("expected duplicate candidate")
	}
	if candidate.CanonicalListingID != "canonical-1" || candidate.Confidence < 0.90 {
		t.Fatalf("unexpected candidate: %+v", candidate)
	}
}

func TestOnlyOneMatchingPhotoDoesNotAutoDeduplicate(t *testing.T) {
	ctx := context.Background()
	repo := &testImageRepo{}
	service := NewService(NewImageHasher(), repo)
	photo1 := writePatternJPEG(t, "single-match.jpg", 128, 128, 95, patternA)
	if err := service.SaveFingerprint(ctx, "canonical-1", []domain.ApartmentPhoto{{URL: photo1}}); err != nil {
		t.Fatal(err)
	}

	candidate, err := service.CheckDuplicate(ctx, domain.ApartmentListing{
		ID:     "incoming",
		Photos: []domain.ApartmentPhoto{{URL: photo1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate != nil {
		t.Fatalf("expected no auto-deduplication for one image, got %+v", candidate)
	}
}

func TestDifferentImagesNotMarkedDuplicate(t *testing.T) {
	ctx := context.Background()
	repo := &testImageRepo{}
	service := NewService(NewImageHasher(), repo)
	photo1 := writePatternJPEG(t, "different-a.jpg", 128, 128, 95, patternA)
	if err := service.SaveFingerprint(ctx, "canonical-1", []domain.ApartmentPhoto{{URL: photo1}}); err != nil {
		t.Fatal(err)
	}

	photo2 := writePatternJPEG(t, "different-c.jpg", 128, 128, 95, patternC)
	candidate, err := service.CheckDuplicate(ctx, domain.ApartmentListing{
		ID:     "incoming",
		Photos: []domain.ApartmentPhoto{{URL: photo2}, {URL: photo2}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if candidate != nil {
		t.Fatalf("expected different images not to match, got %+v", candidate)
	}
}

func writePatternJPEG(t *testing.T, name string, width, height, quality int, pattern func(int, int) color.Color) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, pattern(x*160/width, y*160/height))
		}
	}
	return writeJPEG(t, name, img, quality)
}

func writeCenterCropJPEG(t *testing.T, name string, width, height int, ratio float64, quality int, pattern func(int, int) color.Color) string {
	t.Helper()
	full := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			full.Set(x, y, pattern(x, y))
		}
	}
	crop := centerCrop(full.Bounds(), ratio)
	out := image.NewRGBA(image.Rect(0, 0, crop.Dx(), crop.Dy()))
	draw.Draw(out, out.Bounds(), full, crop.Min, draw.Src)
	return writeJPEG(t, name, out, quality)
}

func writeJPEG(t *testing.T, name string, img image.Image, quality int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := jpeg.Encode(file, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return path
}

func patternA(x, y int) color.Color {
	if (x/20+y/20)%2 == 0 {
		return color.RGBA{R: 240, G: 240, B: 240, A: 255}
	}
	return color.RGBA{R: 30, G: 30, B: 30, A: 255}
}

func patternB(x, y int) color.Color {
	if x > y {
		return color.RGBA{R: 230, G: 230, B: 230, A: 255}
	}
	return color.RGBA{R: 20, G: 20, B: 20, A: 255}
}

func patternC(x, y int) color.Color {
	if (x/10)%2 == 0 {
		return color.RGBA{R: 20, G: 20, B: 20, A: 255}
	}
	return color.RGBA{R: 230, G: 230, B: 230, A: 255}
}

type testImageRepo struct {
	images     []StoredListingImage
	duplicates []ListingDuplicateRecord
}

func (r *testImageRepo) SaveImageFingerprints(ctx context.Context, listingID string, fingerprints []ListingImageFingerprint) error {
	for _, fingerprint := range fingerprints {
		r.images = append(r.images, StoredListingImage{
			ListingID:      listingID,
			SourceImageURL: fingerprint.ImageURL,
			Hashes:         fingerprint.Hashes,
		})
	}
	return nil
}

func (r *testImageRepo) ListImageFingerprints(ctx context.Context) ([]StoredListingImage, error) {
	out := make([]StoredListingImage, len(r.images))
	copy(out, r.images)
	return out, nil
}

func (r *testImageRepo) SaveDuplicate(ctx context.Context, listingID string, candidate DuplicateCandidate) error {
	r.duplicates = append(r.duplicates, ListingDuplicateRecord{
		ListingID:          listingID,
		CanonicalListingID: candidate.CanonicalListingID,
		Confidence:         candidate.Confidence,
		Reason:             candidate.Reason,
	})
	return nil
}

func (r *testImageRepo) ListDuplicates(ctx context.Context) ([]ListingDuplicateRecord, error) {
	out := make([]ListingDuplicateRecord, len(r.duplicates))
	copy(out, r.duplicates)
	return out, nil
}
