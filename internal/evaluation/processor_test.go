package evaluation

import (
	"context"
	"testing"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/sources"
)

type countingPhotoLoader struct {
	calls int
}

func (l *countingPhotoLoader) LoadPhotos(ctx context.Context, message sources.ChannelMessage, listing domain.ApartmentListing) (domain.ApartmentListing, error) {
	l.calls++
	return listing, nil
}

func TestProcessMessageSkipsPhotoLoadingWhenBasicFiltersFail(t *testing.T) {
	loader := &countingPhotoLoader{}
	processor := Processor{
		Prefs:       domain.UserPreferences{MaxBudget: 600},
		PhotoLoader: loader,
	}

	result, err := processor.ProcessMessage(context.Background(), domain.ListingSourceRef{}, sources.ChannelMessage{
		Text: "Apartment\n700 USD\n50 m2",
		Photos: []sources.ChannelPhoto{{
			ID: "telegram-photo-location:test-token",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Record.Status != "rejected_basic" {
		t.Fatalf("expected rejected_basic, got %s", result.Record.Status)
	}
	if loader.calls != 0 {
		t.Fatalf("expected photo loader not to be called, got %d calls", loader.calls)
	}
}
