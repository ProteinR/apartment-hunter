package telegramuserbot

import (
	"context"
	"testing"
	"time"

	"apartment-hunter/internal/sources"
)

func TestCoalesceGroupedMessagesUsesCaptionMessageAsCanonical(t *testing.T) {
	messages := []sources.ChannelMessage{
		{
			ChannelID:       "domikibatumi",
			ChannelUsername: "domikibatumi",
			MessageID:       952572,
			GroupedID:       123,
			Photos:          []sources.ChannelPhoto{{ID: "photo-3"}},
			Link:            "https://t.me/domikibatumi/952572",
		},
		{
			ChannelID:       "domikibatumi",
			ChannelUsername: "domikibatumi",
			MessageID:       952571,
			GroupedID:       123,
			Photos:          []sources.ChannelPhoto{{ID: "photo-2"}},
			Link:            "https://t.me/domikibatumi/952571",
		},
		{
			ChannelID:       "domikibatumi",
			ChannelUsername: "domikibatumi",
			MessageID:       952563,
			GroupedID:       123,
			Text:            "Сдается квартира\n55 м²\n500 $",
			Photos:          []sources.ChannelPhoto{{ID: "photo-1", Caption: "Сдается квартира\n55 м²\n500 $"}},
			Link:            "https://t.me/domikibatumi/952563",
		},
		{
			ChannelID: "domikibatumi",
			MessageID: 952463,
			Text:      "not an album",
		},
	}

	grouped := coalesceGroupedMessages(messages)
	if len(grouped) != 2 {
		t.Fatalf("expected 2 messages after grouping, got %d: %+v", len(grouped), grouped)
	}

	album := grouped[0]
	if album.MessageID != 952563 {
		t.Fatalf("expected caption message id to be canonical, got %d", album.MessageID)
	}
	if album.Link != "https://t.me/domikibatumi/952563" {
		t.Fatalf("expected caption message link, got %q", album.Link)
	}
	if album.Text != "Сдается квартира\n55 м²\n500 $" {
		t.Fatalf("unexpected album text %q", album.Text)
	}
	if len(album.Photos) != 3 {
		t.Fatalf("expected 3 album photos, got %+v", album.Photos)
	}
	if album.Photos[0].ID != "photo-3" || album.Photos[1].ID != "photo-2" || album.Photos[2].ID != "photo-1" {
		t.Fatalf("unexpected photo order %+v", album.Photos)
	}

	if grouped[1].MessageID != 952463 || grouped[1].Text != "not an album" {
		t.Fatalf("unexpected standalone message %+v", grouped[1])
	}
}

func TestGroupedMessageHandlerFlushesMergedAlbum(t *testing.T) {
	var handled []sources.ChannelMessage
	handler := newGroupedMessageHandler(func(ctx context.Context, message sources.ChannelMessage) error {
		handled = append(handled, message)
		return nil
	}, time.Hour)

	for _, message := range []sources.ChannelMessage{
		{
			ChannelID: "domikibatumi",
			MessageID: 2,
			GroupedID: 99,
			Photos:    []sources.ChannelPhoto{{ID: "photo-2"}},
		},
		{
			ChannelID: "domikibatumi",
			MessageID: 1,
			GroupedID: 99,
			Text:      "Сдается квартира\n50 м²\n500 $",
			Photos:    []sources.ChannelPhoto{{ID: "photo-1"}},
		},
	} {
		if err := handler.Handle(context.Background(), message); err != nil {
			t.Fatal(err)
		}
	}
	if len(handled) != 0 {
		t.Fatalf("expected grouped messages to wait for flush, got %+v", handled)
	}

	if err := handler.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(handled) != 1 {
		t.Fatalf("expected one merged album, got %+v", handled)
	}
	if handled[0].MessageID != 1 || handled[0].Text == "" || len(handled[0].Photos) != 2 {
		t.Fatalf("unexpected merged album %+v", handled[0])
	}
}

func TestGroupedMessageHandlerPassesStandaloneImmediately(t *testing.T) {
	var handled []sources.ChannelMessage
	handler := newGroupedMessageHandler(func(ctx context.Context, message sources.ChannelMessage) error {
		handled = append(handled, message)
		return nil
	}, time.Hour)

	err := handler.Handle(context.Background(), sources.ChannelMessage{
		ChannelID: "domikibatumi",
		MessageID: 7,
		Text:      "standalone",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(handled) != 1 || handled[0].MessageID != 7 {
		t.Fatalf("expected standalone message to be handled immediately, got %+v", handled)
	}
}
