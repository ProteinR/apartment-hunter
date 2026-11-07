package access

import (
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	StatusAllowed Status = "allowed"
	StatusPending Status = "pending"
	StatusDenied  Status = "denied"
)

type Entry struct {
	TelegramChatID int64
	Username       string
	DisplayName    string
	Status         Status
	UpdatedAt      time.Time
}

func NormalizeUsername(username string) string {
	return strings.ToLower(strings.TrimPrefix(strings.TrimSpace(username), "@"))
}

func (e Entry) Label() string {
	var parts []string
	if e.DisplayName != "" {
		parts = append(parts, e.DisplayName)
	}
	if e.Username != "" {
		parts = append(parts, "@"+e.Username)
	}
	if e.TelegramChatID != 0 {
		parts = append(parts, "id "+strconv.FormatInt(e.TelegramChatID, 10))
	}
	if len(parts) == 0 {
		return "неизвестный пользователь"
	}
	return strings.Join(parts, ", ")
}
