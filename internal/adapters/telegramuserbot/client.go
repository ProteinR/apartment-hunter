package telegramuserbot

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/sources"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

type Config struct {
	AppID         int
	AppHash       string
	Phone         string
	Password      string
	SessionPath   string
	SessionString string
	MediaDir      string
	CodeReader    io.Reader
}

type Client struct {
	cfg Config
}

func FromEnv(codeReader io.Reader) (*Client, error) {
	appID, err := strconv.Atoi(strings.TrimSpace(os.Getenv("TELEGRAM_API_ID")))
	if err != nil || appID == 0 {
		return nil, fmt.Errorf("TELEGRAM_API_ID is required")
	}
	cfg := Config{
		AppID:         appID,
		AppHash:       strings.TrimSpace(os.Getenv("TELEGRAM_API_HASH")),
		Phone:         strings.TrimSpace(os.Getenv("TELEGRAM_PHONE")),
		Password:      os.Getenv("TELEGRAM_PASSWORD"),
		SessionPath:   strings.TrimSpace(os.Getenv("TELEGRAM_SESSION_FILE")),
		SessionString: strings.TrimSpace(os.Getenv("TELEGRAM_SESSION_STRING")),
		MediaDir:      strings.TrimSpace(os.Getenv("TELEGRAM_MEDIA_DIR")),
		CodeReader:    codeReader,
	}
	if cfg.AppHash == "" {
		return nil, fmt.Errorf("TELEGRAM_API_HASH is required")
	}
	if cfg.Phone == "" && cfg.SessionString == "" {
		return nil, fmt.Errorf("TELEGRAM_PHONE is required")
	}
	if cfg.SessionPath == "" {
		cfg.SessionPath = ".telegram/userbot.session"
	}
	if cfg.MediaDir == "" {
		cfg.MediaDir = ".telegram/media"
	}
	return New(cfg), nil
}

func New(cfg Config) *Client {
	if cfg.CodeReader == nil {
		cfg.CodeReader = os.Stdin
	}
	return &Client{cfg: cfg}
}

func (c *Client) Fetch(ctx context.Context, channel sources.ChannelRef, cursor sources.Cursor, limit int) ([]sources.ChannelMessage, sources.Cursor, error) {
	if limit <= 0 {
		limit = 50
	}
	storage, err := c.sessionStorage(ctx)
	if err != nil {
		return nil, cursor, err
	}

	client := telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, telegram.Options{
		SessionStorage: storage,
		NoUpdates:      true,
	})

	var messages []sources.ChannelMessage
	next := cursor
	err = client.Run(ctx, func(ctx context.Context) error {
		if err := c.authIfNeeded(ctx, client); err != nil {
			return err
		}
		api := client.API()
		peer, resolved, err := resolveChannel(ctx, api, channel)
		if err != nil {
			return err
		}

		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:  peer,
			Limit: limit,
			MinID: cursor.LastMessageID,
		})
		if err != nil {
			return err
		}
		modified, ok := history.AsModified()
		if !ok {
			return nil
		}
		for _, raw := range modified.GetMessages() {
			message, ok := raw.(*tg.Message)
			if !ok || message.ID <= cursor.LastMessageID {
				continue
			}
			mapped := mapMessage(channel, resolved, message)
			messages = append(messages, mapped)
			if message.ID > next.LastMessageID {
				next.LastMessageID = message.ID
			}
		}
		return nil
	})
	if err != nil {
		return nil, cursor, err
	}
	return messages, next, nil
}

func (c *Client) sessionStorage(ctx context.Context) (session.Storage, error) {
	if sessionString := strings.TrimSpace(c.cfg.SessionString); sessionString != "" {
		data, err := session.TelethonSession(sessionString)
		if err != nil {
			return nil, fmt.Errorf("decode TELEGRAM_SESSION_STRING: %w", err)
		}
		storage := &session.StorageMemory{}
		if err := (&session.Loader{Storage: storage}).Save(ctx, data); err != nil {
			return nil, err
		}
		return storage, nil
	}
	if err := os.MkdirAll(filepath.Dir(c.cfg.SessionPath), 0755); err != nil {
		return nil, err
	}
	return &session.FileStorage{Path: c.cfg.SessionPath}, nil
}

func (c *Client) authIfNeeded(ctx context.Context, client *telegram.Client) error {
	status, err := client.Auth().Status(ctx)
	if err != nil {
		return err
	}
	if status.Authorized {
		return nil
	}

	codeAsk := func(ctx context.Context, sentCode *tg.AuthSentCode) (string, error) {
		fmt.Print("Telegram code: ")
		code, err := bufio.NewReader(c.cfg.CodeReader).ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(code), nil
	}

	var authenticator auth.UserAuthenticator
	if c.cfg.Password == "" {
		authenticator = auth.CodeOnly(c.cfg.Phone, auth.CodeAuthenticatorFunc(codeAsk))
	} else {
		authenticator = auth.Constant(c.cfg.Phone, c.cfg.Password, auth.CodeAuthenticatorFunc(codeAsk))
	}
	return auth.NewFlow(authenticator, auth.SendCodeOptions{}).Run(ctx, client.Auth())
}

type resolvedChannel struct {
	ID       string
	Title    string
	Username string
}

func resolveChannel(ctx context.Context, api *tg.Client, channel sources.ChannelRef) (tg.InputPeerClass, resolvedChannel, error) {
	username := strings.TrimPrefix(strings.TrimSpace(channel.Username), "@")
	if username == "" {
		return nil, resolvedChannel{}, fmt.Errorf("telegram channel username is required for %q", channel.ID)
	}

	peer, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, resolvedChannel{}, err
	}
	for _, chat := range peer.Chats {
		if ch, ok := chat.(*tg.Channel); ok {
			return ch.AsInputPeer(), resolvedChannel{
				ID:       strconv.FormatInt(ch.ID, 10),
				Title:    ch.Title,
				Username: username,
			}, nil
		}
	}
	return nil, resolvedChannel{}, fmt.Errorf("resolved %q but found no channel", username)
}

func mapMessage(ref sources.ChannelRef, resolved resolvedChannel, message *tg.Message) sources.ChannelMessage {
	channelID := ref.ID
	if channelID == "" {
		channelID = resolved.ID
	}
	channelName := ref.Title
	if channelName == "" {
		channelName = resolved.Title
	}

	groupedID, _ := message.GetGroupedID()
	return sources.ChannelMessage{
		ChannelID:   channelID,
		ChannelName: channelName,
		MessageID:   message.ID,
		Date:        time.Unix(int64(message.Date), 0),
		Text:        message.Message,
		GroupedID:   groupedID,
		Photos:      messagePhotos(message),
		Link:        messageLink(resolved.Username, message.ID),
	}
}

func messagePhotos(message *tg.Message) []sources.ChannelPhoto {
	media, ok := message.GetMedia()
	if !ok {
		return nil
	}
	photoMedia, ok := media.(*tg.MessageMediaPhoto)
	if !ok || photoMedia.Photo == nil {
		return nil
	}
	photo, ok := photoMedia.Photo.(*tg.Photo)
	if !ok {
		return nil
	}
	token := photoDownloadToken(photo, message.ID)
	if token == "" {
		return nil
	}
	return []sources.ChannelPhoto{{
		ID:      token,
		Caption: message.Message,
	}}
}

func (c *Client) LoadPhotos(ctx context.Context, message sources.ChannelMessage, listing domain.ApartmentListing) (domain.ApartmentListing, error) {
	photos, err := c.DownloadPhotos(ctx, message)
	if err != nil {
		return listing, err
	}
	if len(photos) == 0 {
		return listing, nil
	}
	listing.Photos = make([]domain.ApartmentPhoto, 0, len(photos))
	for _, photo := range photos {
		listing.Photos = append(listing.Photos, domain.ApartmentPhoto{
			URL:     photo.ID,
			Caption: photo.Caption,
		})
	}
	return listing, nil
}

func (c *Client) DownloadPhotos(ctx context.Context, message sources.ChannelMessage) ([]sources.ChannelPhoto, error) {
	tokens := make([]photoLocationToken, 0, len(message.Photos))
	for _, photo := range message.Photos {
		token, err := parsePhotoDownloadToken(photo.ID)
		if err != nil || token.PhotoID == 0 {
			continue
		}
		tokens = append(tokens, token)
	}
	if len(tokens) == 0 {
		return nil, nil
	}

	storage, err := c.sessionStorage(ctx)
	if err != nil {
		return nil, err
	}
	client := telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, telegram.Options{
		SessionStorage: storage,
		NoUpdates:      true,
	})

	var downloaded []sources.ChannelPhoto
	err = client.Run(ctx, func(ctx context.Context) error {
		if err := c.authIfNeeded(ctx, client); err != nil {
			return err
		}
		for i, token := range tokens {
			location := &tg.InputPhotoFileLocation{
				ID:            token.PhotoID,
				AccessHash:    token.AccessHash,
				FileReference: token.FileReference,
				ThumbSize:     token.ThumbSize,
			}
			channelID := message.ChannelID
			if channelID == "" {
				channelID = "unknown"
			}
			dir := filepath.Join(c.cfg.MediaDir, safePathPart(channelID))
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}
			path := filepath.Join(dir, fmt.Sprintf("%d_%d_%s.jpg", message.MessageID, token.PhotoID, safePathPart(token.ThumbSize)))
			if _, err := client.Download(location).ToPath(ctx, path); err != nil {
				return fmt.Errorf("download telegram photo %d from message %d: %w", token.PhotoID, message.MessageID, err)
			}
			caption := ""
			if i < len(message.Photos) {
				caption = message.Photos[i].Caption
			}
			downloaded = append(downloaded, sources.ChannelPhoto{
				ID:      "file://" + path,
				Caption: caption,
			})
		}
		return nil
	})
	return downloaded, err
}

func photoDownloadLocation(photo *tg.Photo) (*tg.InputPhotoFileLocation, string) {
	thumbSize := largestPhotoSize(photo.Sizes)
	if thumbSize == "" {
		return nil, ""
	}
	return &tg.InputPhotoFileLocation{
		ID:            photo.ID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     thumbSize,
	}, thumbSize
}

func largestPhotoSize(sizes []tg.PhotoSizeClass) string {
	bestType := ""
	bestArea := 0
	for _, raw := range sizes {
		size, ok := raw.(interface {
			GetType() string
			GetW() int
			GetH() int
		})
		if !ok {
			continue
		}
		area := size.GetW() * size.GetH()
		if area > bestArea {
			bestArea = area
			bestType = size.GetType()
		}
	}
	return bestType
}

const photoTokenPrefix = "telegram-photo-location:"

type photoLocationToken struct {
	PhotoID       int64  `json:"photo_id"`
	MessageID     int    `json:"message_id"`
	AccessHash    int64  `json:"access_hash"`
	FileReference []byte `json:"file_reference"`
	ThumbSize     string `json:"thumb_size"`
}

func photoDownloadToken(photo *tg.Photo, messageID int) string {
	location, thumbSize := photoDownloadLocation(photo)
	if location == nil {
		return ""
	}
	data, err := json.Marshal(photoLocationToken{
		PhotoID:       photo.ID,
		MessageID:     messageID,
		AccessHash:    photo.AccessHash,
		FileReference: photo.FileReference,
		ThumbSize:     thumbSize,
	})
	if err != nil {
		return ""
	}
	return photoTokenPrefix + base64.RawURLEncoding.EncodeToString(data)
}

func parsePhotoDownloadToken(value string) (photoLocationToken, error) {
	if !strings.HasPrefix(value, photoTokenPrefix) {
		return photoLocationToken{}, fmt.Errorf("not a telegram photo token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, photoTokenPrefix))
	if err != nil {
		return photoLocationToken{}, err
	}
	var token photoLocationToken
	if err := json.Unmarshal(raw, &token); err != nil {
		return photoLocationToken{}, err
	}
	return token, nil
}

var unsafePathPart = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func safePathPart(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	value = unsafePathPart.ReplaceAllString(value, "_")
	value = strings.Trim(value, "._-")
	if value == "" {
		return "unknown"
	}
	return value
}

func messageLink(username string, messageID int) string {
	if username == "" || messageID == 0 {
		return ""
	}
	return "https://t.me/" + username + "/" + strconv.Itoa(messageID)
}
