package profiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/sources"
)

type Config struct {
	Profiles []Profile `json:"profiles"`
}

type Profile struct {
	ID              string                  `json:"id"`
	TelegramChatID  int64                   `json:"telegram_chat_id,omitempty"`
	PreferencesPath string                  `json:"preferences_path"`
	Preferences     *domain.UserPreferences `json:"preferences,omitempty"`
	Channels        []ChannelSubscription   `json:"channels"`
	DebugMode       *bool                   `json:"debug_mode,omitempty"`
	MCPMode         *bool                   `json:"mcp_mode,omitempty"`
	FilterDisabled  *bool                   `json:"filter_disabled,omitempty"`
	MCPHistory      []MCPMessage            `json:"mcp_history,omitempty"`
}

type MCPMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type ChannelSubscription struct {
	Source sources.ChannelRef `json:"source"`
	Cursor sources.Cursor     `json:"cursor"`
	Limit  int                `json:"limit,omitempty"`
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Config{}, nil
		}
		return Config{}, err
	}
	if len(data) == 0 {
		return Config{}, nil
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func (c *Config) Upsert(profile Profile) {
	for i := range c.Profiles {
		if c.Profiles[i].ID == profile.ID {
			if profile.TelegramChatID != 0 {
				c.Profiles[i].TelegramChatID = profile.TelegramChatID
			}
			if profile.PreferencesPath != "" {
				c.Profiles[i].PreferencesPath = profile.PreferencesPath
			}
			if profile.Preferences != nil {
				c.Profiles[i].Preferences = profile.Preferences
			}
			if profile.Channels != nil {
				c.Profiles[i].Channels = profile.Channels
			}
			if profile.DebugMode != nil {
				c.Profiles[i].DebugMode = profile.DebugMode
			}
			if profile.MCPMode != nil {
				c.Profiles[i].MCPMode = profile.MCPMode
			}
			if profile.FilterDisabled != nil {
				c.Profiles[i].FilterDisabled = profile.FilterDisabled
			}
			return
		}
	}
	c.Profiles = append(c.Profiles, profile)
}

func (c Config) Find(id string) (Profile, bool) {
	for _, profile := range c.Profiles {
		if profile.ID == id {
			return profile, true
		}
	}
	return Profile{}, false
}

func (c *Config) UpsertChannel(profileID string, sub ChannelSubscription) bool {
	for profileIndex := range c.Profiles {
		if c.Profiles[profileIndex].ID != profileID {
			continue
		}
		if sub.Limit <= 0 {
			sub.Limit = 50
		}
		sub.Source.Username = strings.TrimPrefix(strings.TrimSpace(sub.Source.Username), "@")
		if sub.Source.ID == "" {
			sub.Source.ID = sub.Source.Username
		}
		for channelIndex := range c.Profiles[profileIndex].Channels {
			existing := &c.Profiles[profileIndex].Channels[channelIndex]
			if sameChannel(existing.Source, sub.Source) {
				existing.Source = sub.Source
				if sub.Limit > 0 {
					existing.Limit = sub.Limit
				}
				return false
			}
		}
		c.Profiles[profileIndex].Channels = append(c.Profiles[profileIndex].Channels, sub)
		return true
	}
	return false
}

func (c *Config) RemoveChannel(profileID, channel string) bool {
	channel = strings.TrimPrefix(strings.TrimSpace(channel), "@")
	for profileIndex := range c.Profiles {
		if c.Profiles[profileIndex].ID != profileID {
			continue
		}
		channels := c.Profiles[profileIndex].Channels
		for channelIndex, sub := range channels {
			if strings.EqualFold(sub.Source.ID, channel) || strings.EqualFold(sub.Source.Username, channel) {
				c.Profiles[profileIndex].Channels = append(channels[:channelIndex], channels[channelIndex+1:]...)
				return true
			}
		}
	}
	return false
}

func sameChannel(left, right sources.ChannelRef) bool {
	if left.ID != "" && right.ID != "" && strings.EqualFold(left.ID, right.ID) {
		return true
	}
	if left.Username != "" && right.Username != "" && strings.EqualFold(left.Username, right.Username) {
		return true
	}
	return false
}

func IDForTelegramChat(chatID int64) string {
	return "telegram_" + strconv.FormatInt(chatID, 10)
}

func DebugModeEnabled(profile Profile) bool {
	if profile.DebugMode == nil {
		return true
	}
	return *profile.DebugMode
}

func FilterDisabled(profile Profile) bool {
	if profile.FilterDisabled == nil {
		return false
	}
	return *profile.FilterDisabled
}

type PopularSource struct {
	Username string
	Title    string
	Users    int
}

type QuietSearch struct {
	ProfileID      string
	TelegramChatID int64
	QuietSince     time.Time
	StatusCounts   map[string]int
}
