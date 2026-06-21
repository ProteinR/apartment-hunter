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
	"sync"
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
	mu  sync.Mutex
	tg  *telegram.Client
}

type MessageHandler func(ctx context.Context, message sources.ChannelMessage) error

const albumSettleDelay = 1500 * time.Millisecond

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

func (c *Client) Listen(ctx context.Context, handler MessageHandler) error {
	if handler == nil {
		return fmt.Errorf("telegram message handler is required")
	}
	storage, err := c.sessionStorage(ctx)
	if err != nil {
		return err
	}

	dispatcher := tg.NewUpdateDispatcher()
	client := telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, telegram.Options{
		SessionStorage: storage,
		UpdateHandler:  dispatcher,
	})

	albumHandler := newGroupedMessageHandler(handler, albumSettleDelay)
	handleMessage := func(ctx context.Context, entities tg.Entities, raw tg.MessageClass) error {
		message, ok := processableUpdateMessage(raw)
		if !ok {
			return nil
		}
		mapped := mapUpdateMessage(entities, message)
		if mapped.ChannelID == "" && mapped.ChannelUsername == "" {
			return nil
		}
		return albumHandler.Handle(ctx, mapped)
	}
	dispatcher.OnNewMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateNewMessage) error {
		return handleMessage(ctx, entities, update.Message)
	})
	dispatcher.OnNewChannelMessage(func(ctx context.Context, entities tg.Entities, update *tg.UpdateNewChannelMessage) error {
		return handleMessage(ctx, entities, update.Message)
	})

	c.mu.Lock()
	c.tg = client
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.tg = nil
		c.mu.Unlock()
	}()

	return client.Run(ctx, func(ctx context.Context) error {
		if err := c.authIfNeeded(ctx, client); err != nil {
			return err
		}
		<-ctx.Done()
		_ = albumHandler.Flush(context.Background())
		return nil
	})
}

func processableUpdateMessage(raw tg.MessageClass) (*tg.Message, bool) {
	message, ok := raw.(*tg.Message)
	if !ok {
		return nil, false
	}
	return message, true
}

func (c *Client) ListAvailableChats(ctx context.Context, limit int) ([]sources.AvailableChat, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	storage, err := c.sessionStorage(ctx)
	if err != nil {
		return nil, err
	}

	client := telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, telegram.Options{
		SessionStorage: storage,
		NoUpdates:      true,
	})

	var chats []sources.AvailableChat
	err = client.Run(ctx, func(ctx context.Context) error {
		if err := c.authIfNeeded(ctx, client); err != nil {
			return err
		}
		dialogs, err := client.API().MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetPeer: &tg.InputPeerEmpty{},
			Limit:      limit,
		})
		if err != nil {
			return err
		}
		modified, ok := dialogs.AsModified()
		if !ok {
			return nil
		}
		chats = publicAvailableChats(modified.GetChats())
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chats, nil
}

func publicAvailableChats(rawChats []tg.ChatClass) []sources.AvailableChat {
	available := make([]sources.AvailableChat, 0, len(rawChats))
	for _, raw := range rawChats {
		channel, ok := raw.(*tg.Channel)
		if !ok || channel.Left || (!channel.Broadcast && !channel.Megagroup) {
			continue
		}
		username, ok := channel.GetUsername()
		if !ok || strings.TrimSpace(username) == "" {
			continue
		}
		kind := "group"
		if channel.Broadcast {
			kind = "channel"
		}
		available = append(available, sources.AvailableChat{
			ID:       strconv.FormatInt(channel.ID, 10),
			Title:    channel.Title,
			Username: username,
			Kind:     kind,
		})
	}
	return available
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
	return coalesceGroupedMessages(messages), next, nil
}

// FetchAlbumText looks up the text of a grouped album by scanning sibling messages
// that share the same groupedID around the anchor message. Telegram puts an album's
// caption on a single message of the group, so a photo-only fragment can arrive without
// it. Returns the first non-empty sibling text found, or "" if none.
func (c *Client) FetchAlbumText(ctx context.Context, channel sources.ChannelRef, groupedID int64, anchorMessageID int) (string, error) {
	if groupedID == 0 || anchorMessageID <= 0 {
		return "", nil
	}
	storage, err := c.sessionStorage(ctx)
	if err != nil {
		return "", err
	}
	client := telegram.NewClient(c.cfg.AppID, c.cfg.AppHash, telegram.Options{
		SessionStorage: storage,
		NoUpdates:      true,
	})
	const albumWindow = 20
	text := ""
	err = client.Run(ctx, func(ctx context.Context) error {
		if err := c.authIfNeeded(ctx, client); err != nil {
			return err
		}
		api := client.API()
		peer, _, err := resolveChannel(ctx, api, channel)
		if err != nil {
			return err
		}
		// Center the window on the anchor: OffsetID points just past the newest
		// message we want, AddOffset pulls in messages newer than the offset too.
		history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer:      peer,
			OffsetID:  anchorMessageID + albumWindow,
			AddOffset: 0,
			Limit:     albumWindow * 2,
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
			if !ok {
				continue
			}
			gid, _ := message.GetGroupedID()
			if gid != groupedID {
				continue
			}
			if strings.TrimSpace(message.Message) != "" {
				text = message.Message
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return text, nil
}

type groupedMessageHandler struct {
	handler MessageHandler
	delay   time.Duration

	mu      sync.Mutex
	pending map[groupedMessageKey]*pendingGroupedMessage
}

type pendingGroupedMessage struct {
	ctx      context.Context
	message  sources.ChannelMessage
	timer    *time.Timer
	flushing bool
}

type groupedMessageKey struct {
	channelID       string
	channelUsername string
	groupedID       int64
}

func newGroupedMessageHandler(handler MessageHandler, delay time.Duration) *groupedMessageHandler {
	return &groupedMessageHandler{
		handler: handler,
		delay:   delay,
		pending: map[groupedMessageKey]*pendingGroupedMessage{},
	}
}

func (h *groupedMessageHandler) Handle(ctx context.Context, message sources.ChannelMessage) error {
	if message.GroupedID == 0 {
		return h.handler(ctx, message)
	}

	key := groupedKey(message)
	h.mu.Lock()
	pending := h.pending[key]
	if pending == nil {
		pending = &pendingGroupedMessage{ctx: ctx, message: message}
		h.pending[key] = pending
	} else {
		pending.message = mergeGroupedMessage(pending.message, message)
	}
	if pending.timer != nil {
		pending.timer.Stop()
	}
	pending.timer = time.AfterFunc(h.delay, func() {
		_ = h.flushKey(key)
	})
	h.mu.Unlock()
	return nil
}

func (h *groupedMessageHandler) Flush(ctx context.Context) error {
	h.mu.Lock()
	keys := make([]groupedMessageKey, 0, len(h.pending))
	for key := range h.pending {
		keys = append(keys, key)
	}
	h.mu.Unlock()

	for _, key := range keys {
		if err := h.flushKeyWithContext(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (h *groupedMessageHandler) flushKey(key groupedMessageKey) error {
	return h.flushKeyWithContext(nil, key)
}

func (h *groupedMessageHandler) flushKeyWithContext(ctx context.Context, key groupedMessageKey) error {
	h.mu.Lock()
	pending := h.pending[key]
	if pending == nil || pending.flushing {
		h.mu.Unlock()
		return nil
	}
	pending.flushing = true
	delete(h.pending, key)
	if pending.timer != nil {
		pending.timer.Stop()
	}
	message := pending.message
	if ctx == nil && pending.ctx != nil {
		ctx = pending.ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.mu.Unlock()

	return h.handler(ctx, message)
}

func coalesceGroupedMessages(messages []sources.ChannelMessage) []sources.ChannelMessage {
	out := make([]sources.ChannelMessage, 0, len(messages))
	indexByKey := map[groupedMessageKey]int{}
	for _, message := range messages {
		if message.GroupedID == 0 {
			out = append(out, message)
			continue
		}
		key := groupedKey(message)
		index, ok := indexByKey[key]
		if !ok {
			indexByKey[key] = len(out)
			out = append(out, message)
			continue
		}
		out[index] = mergeGroupedMessage(out[index], message)
	}
	return out
}

func groupedKey(message sources.ChannelMessage) groupedMessageKey {
	return groupedMessageKey{
		channelID:       message.ChannelID,
		channelUsername: message.ChannelUsername,
		groupedID:       message.GroupedID,
	}
}

func mergeGroupedMessage(base, next sources.ChannelMessage) sources.ChannelMessage {
	base.Photos = append(base.Photos, next.Photos...)
	if strings.TrimSpace(base.Text) == "" && strings.TrimSpace(next.Text) != "" {
		photos := base.Photos
		base = next
		base.Photos = photos
	}
	if base.Date.IsZero() || (!next.Date.IsZero() && next.Date.Before(base.Date)) {
		base.Date = next.Date
	}
	return base
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
	channelID := resolved.ID
	if channelID == "" {
		channelID = ref.ID
	}
	channelName := ref.Title
	if channelName == "" {
		channelName = resolved.Title
	}

	groupedID, _ := message.GetGroupedID()
	return sources.ChannelMessage{
		ChannelID:       channelID,
		ChannelName:     channelName,
		ChannelUsername: resolved.Username,
		MessageID:       message.ID,
		Date:            time.Unix(int64(message.Date), 0),
		Text:            message.Message,
		GroupedID:       groupedID,
		Photos:          messagePhotos(message),
		Link:            messageLink(resolved.Username, message.ID),
	}
}

func mapUpdateMessage(entities tg.Entities, message *tg.Message) sources.ChannelMessage {
	channelID, channelName, username := peerMetadata(entities, message.GetPeerID())
	groupedID, _ := message.GetGroupedID()
	return sources.ChannelMessage{
		ChannelID:       channelID,
		ChannelName:     channelName,
		ChannelUsername: username,
		MessageID:       message.ID,
		Date:            time.Unix(int64(message.Date), 0),
		Text:            message.Message,
		GroupedID:       groupedID,
		Photos:          messagePhotos(message),
		Link:            messageLink(username, message.ID),
	}
}

func peerMetadata(entities tg.Entities, peer tg.PeerClass) (string, string, string) {
	switch p := peer.(type) {
	case *tg.PeerChannel:
		id := strconv.FormatInt(p.ChannelID, 10)
		channel := entities.Channels[p.ChannelID]
		if channel == nil {
			return id, "", ""
		}
		username, _ := channel.GetUsername()
		return id, channel.Title, username
	case *tg.PeerChat:
		id := strconv.FormatInt(p.ChatID, 10)
		chat := entities.Chats[p.ChatID]
		if chat == nil {
			return id, "", ""
		}
		return id, chat.Title, ""
	default:
		return "", "", ""
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
