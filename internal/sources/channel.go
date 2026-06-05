package sources

import (
	"context"
	"time"
)

type ChannelMessage struct {
	ChannelID   string
	ChannelName string
	MessageID   int
	Date        time.Time
	Text        string
	GroupedID   int64
	Photos      []ChannelPhoto
	Link        string
}

type ChannelPhoto struct {
	ID      string
	Caption string
}

type ChannelReader interface {
	Fetch(ctx context.Context, channel ChannelRef, cursor Cursor, limit int) ([]ChannelMessage, Cursor, error)
}

type ChannelRef struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Username string `json:"username,omitempty"`
}

type Cursor struct {
	LastMessageID int `json:"last_message_id"`
}
