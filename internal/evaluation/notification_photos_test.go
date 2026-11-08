package evaluation

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/sources"
)

type fileWritingPhotoLoader struct {
	dir   string
	paths []string
}

func (l *fileWritingPhotoLoader) LoadPhotos(ctx context.Context, message sources.ChannelMessage, listing domain.ApartmentListing) (domain.ApartmentListing, error) {
	listing.Photos = nil
	for i := range message.Photos {
		path := filepath.Join(l.dir, "photo"+string(rune('a'+i))+".jpg")
		if err := os.WriteFile(path, []byte{0xff, 0xd8, byte(i)}, 0o644); err != nil {
			return listing, err
		}
		l.paths = append(l.paths, path)
		listing.Photos = append(listing.Photos, domain.ApartmentPhoto{URL: "file://" + path})
	}
	return listing, nil
}

func TestProcessMessageReturnsDownloadedPhotosForPassedNotification(t *testing.T) {
	loader := &fileWritingPhotoLoader{dir: t.TempDir()}
	pipelineLog := &fakePipelineLogger{}
	processor := Processor{
		PipelineLog: pipelineLog,
		Prefs:       domain.UserPreferences{MaxBudget: 800},
		Location:    &fakeLocationMatcher{matches: true},
		PhotoLoader: loader,
		Analyzer:    fakeAnalyzer{},
	}
	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{ProfileID: "telegram_42"}, sources.ChannelMessage{
		Text:   "Flat\n650 USD\n50 m2\n41.6511,41.6367",
		Photos: []sources.ChannelPhoto{{ID: "telegram-photo-location:a"}, {ID: "telegram-photo-location:b"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Status != "passed" {
		t.Fatalf("expected passed, got %s (%s)", result.Record.Status, result.Record.FailureReason)
	}
	if len(result.NotificationPhotos) != 2 {
		t.Fatalf("expected 2 notification photos, got %d", len(result.NotificationPhotos))
	}
	savedReason := ""
	for _, event := range pipelineLog.events {
		if event.Stage == "saved" {
			savedReason = event.Reason
		}
	}
	if savedReason != "notification_photos=2/2" {
		t.Fatalf("expected the saved event to report notification photos, got %q", savedReason)
	}
	for _, path := range loader.paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected downloaded photo %s to be removed after processing, stat err %v", path, err)
		}
	}
}
