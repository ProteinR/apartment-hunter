package telegram

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/preferenceparser"
	"apartment-hunter/internal/profiles"
)

//go:embed widgets/search_settings.html
var searchSettingsWidgetHTML string

const (
	widgetSettingsStartPrefix  = "/start set_"
	maxWidgetDistricts         = 12
	maxWidgetDistrictNameRunes = 28
	maxWidgetBudget            = 99999
	maxWidgetAreaM2            = 999
	mustHaveDishwasherBit      = 1
	mustHaveOvenBit            = 2
	settingsWidgetCaption      = "Настройки поиска: подвинь ползунки, отметь пункты и нажми «Сохранить»."
	defaultWidgetEndpoint      = "https://api.telegram.org/beta/bot%s/sendWidget"
	defaultGetMeEndpoint       = "https://api.telegram.org/bot%s/getMe"
)

var widgetSettingsPayloadPattern = regexp.MustCompile(`^set_b(\d{1,5})a(\d{1,3})m([0-3])k(\d{1,5})v([0-9a-f]{8})$`)

const (
	widgetSettingsMalformedText = "Не смог прочитать настройки из виджета, ничего не поменял. Открой настройки заново командой /settings."
	widgetSettingsStaleText     = "Районы поиска поменялись после того, как я прислал этот виджет, поэтому ничего не сохранил. Открой свежий виджет командой /settings."
	widgetSettingsNoPrefsText   = "Сначала расскажи, какую квартиру ищешь, а потом настройки можно будет менять в виджете."
	widgetSettingsUnchangedText = "Настройки из виджета совпадают с текущими, ничего не поменял."
)

type widgetSettings struct {
	MaxBudget         int
	MinAreaM2         int
	MustHave          int
	KeepDistricts     int
	DistrictsChecksum string
}

func isWidgetSettingsStart(text string) bool {
	return strings.HasPrefix(text, widgetSettingsStartPrefix)
}

func parseWidgetSettingsPayload(text string) (widgetSettings, bool) {
	payload := strings.TrimSpace(strings.TrimPrefix(text, "/start "))
	match := widgetSettingsPayloadPattern.FindStringSubmatch(payload)
	if match == nil {
		return widgetSettings{}, false
	}
	budget, _ := strconv.Atoi(match[1])
	area, _ := strconv.Atoi(match[2])
	mustHave, _ := strconv.Atoi(match[3])
	keep, _ := strconv.Atoi(match[4])
	if budget > maxWidgetBudget || area > maxWidgetAreaM2 || keep >= 1<<maxWidgetDistricts {
		return widgetSettings{}, false
	}
	return widgetSettings{
		MaxBudget:         budget,
		MinAreaM2:         area,
		MustHave:          mustHave,
		KeepDistricts:     keep,
		DistrictsChecksum: match[5],
	}, true
}

func widgetDistrictName(location domain.LocationPreference) string {
	if name := strings.TrimSpace(location.Name); name != "" {
		return name
	}
	return strings.TrimSpace(location.CenterName)
}

func widgetDistrictNames(prefs domain.UserPreferences) []string {
	names := make([]string, 0, len(prefs.LocationPreferences))
	for _, location := range prefs.LocationPreferences {
		names = append(names, widgetDistrictName(location))
	}
	return names
}

func widgetDistrictsChecksum(prefs domain.UserPreferences) string {
	return fmt.Sprintf("%08x", crc32.ChecksumIEEE([]byte(strings.Join(widgetDistrictNames(prefs), "\n"))))
}

func shortenedDistrictName(name string) string {
	if utf8.RuneCountInString(name) <= maxWidgetDistrictNameRunes {
		return name
	}
	runes := []rune(name)
	return string(runes[:maxWidgetDistrictNameRunes-1]) + "…"
}

func widgetMustHaveBits(prefs domain.UserPreferences) int {
	bits := 0
	if prefs.RequireDishwasher {
		bits |= mustHaveDishwasherBit
	}
	if prefs.RequireOven {
		bits |= mustHaveOvenBit
	}
	return bits
}

func applyWidgetSettings(stored domain.UserPreferences, settings widgetSettings) (domain.UserPreferences, []string) {
	before := preferenceparser.NormalizePreferences(cloneWidgetPreferences(stored))
	next := cloneWidgetPreferences(before)
	var changes []string

	if settings.MaxBudget != before.MaxBudget {
		changes = append(changes, "Бюджет до: "+budgetLabel(before.MaxBudget)+" → "+budgetLabel(settings.MaxBudget))
	}
	next.MaxBudget = settings.MaxBudget
	next.BudgetMax = optionalInt(settings.MaxBudget)
	if settings.MaxBudget > 0 && next.MinBudget > settings.MaxBudget {
		changes = append(changes, "Бюджет от: "+budgetLabel(next.MinBudget)+" → без нижней границы")
		next.MinBudget = 0
		next.BudgetMin = nil
	}

	area := float64(settings.MinAreaM2)
	if settings.MinAreaM2 == widgetAreaValue(before.MinAreaM2) {
		area = before.MinAreaM2
	}
	if area != before.MinAreaM2 {
		changes = append(changes, "Площадь от: "+areaLabel(before.MinAreaM2)+" → "+areaLabel(area))
	}
	next.MinAreaM2 = area
	next.AreaMin = optionalFloat(area)

	dishwasher := settings.MustHave&mustHaveDishwasherBit != 0
	oven := settings.MustHave&mustHaveOvenBit != 0
	if dishwasher != before.RequireDishwasher {
		changes = append(changes, "Посудомойка: "+requirementLabel(dishwasher))
	}
	if oven != before.RequireOven {
		changes = append(changes, "Духовка: "+requirementLabel(oven))
	}
	next.RequireDishwasher = dishwasher
	next.RequireOven = oven
	next.RequiredAmenities = withoutAmenities(next.RequiredAmenities, "dishwasher", "oven")

	var kept []domain.LocationPreference
	var removed []string
	for i, location := range before.LocationPreferences {
		if i >= maxWidgetDistricts || settings.KeepDistricts&(1<<i) != 0 {
			kept = append(kept, location)
			continue
		}
		removed = append(removed, widgetDistrictName(location))
	}
	if len(removed) > 0 {
		changes = append(changes, "Убрал из поиска: "+strings.Join(removed, ", "))
		next.UnresolvedLocationRules = rulesWithoutDistricts(next.UnresolvedLocationRules, removed)
		next.LocationPreferences = kept
		next.Locations = kept
		if len(kept) == 0 {
			next.LocationPreferences = []domain.LocationPreference{}
			next.Locations = []domain.LocationPreference{}
			next.HasSearchArea = false
		}
	}

	return preferenceparser.NormalizePreferences(next), changes
}

func widgetAreaValue(area float64) int {
	return int(math.Round(area))
}

func rulesWithoutDistricts(rules []string, districts []string) []string {
	var kept []string
	for _, rule := range rules {
		mentionsRemoved := false
		for _, district := range districts {
			if district != "" && strings.Contains(strings.ToLower(rule), strings.ToLower(district)) {
				mentionsRemoved = true
				break
			}
		}
		if !mentionsRemoved {
			kept = append(kept, rule)
		}
	}
	return kept
}

func cloneWidgetPreferences(prefs domain.UserPreferences) domain.UserPreferences {
	prefs.RequiredAmenities = append([]string(nil), prefs.RequiredAmenities...)
	if prefs.LocationPreferences != nil {
		prefs.LocationPreferences = append([]domain.LocationPreference{}, prefs.LocationPreferences...)
	}
	if prefs.Locations != nil {
		prefs.Locations = append([]domain.LocationPreference{}, prefs.Locations...)
	}
	return prefs
}

func withoutAmenities(amenities []string, excluded ...string) []string {
	result := []string{}
	for _, amenity := range amenities {
		skip := false
		for _, value := range excluded {
			if amenity == value {
				skip = true
				break
			}
		}
		if !skip {
			result = append(result, amenity)
		}
	}
	return result
}

func optionalInt(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

func optionalFloat(value float64) *float64 {
	if value <= 0 {
		return nil
	}
	return &value
}

func budgetLabel(value int) string {
	if value <= 0 {
		return "без лимита"
	}
	return fmt.Sprintf("$%d", value)
}

func areaLabel(value float64) string {
	if value <= 0 {
		return "любая"
	}
	return formatWidgetArea(value) + " м²"
}

func formatWidgetArea(value float64) string {
	if value == float64(int(value)) {
		return strconv.Itoa(int(value))
	}
	return strconv.FormatFloat(value, 'f', 1, 64)
}

func requirementLabel(required bool) string {
	if required {
		return "обязательно"
	}
	return "не обязательно"
}

func (b *Bot) applyWidgetSettingsStart(ctx context.Context, chatID int64, text string) (textResponse, error) {
	b.deleteSession(chatID)
	settings, ok := parseWidgetSettingsPayload(text)
	if !ok {
		return textResponse{Text: widgetSettingsMalformedText, ReplyMarkup: removeKeyboard()}, nil
	}
	profile, err := b.ensureProfile(ctx, chatID)
	if err != nil {
		return textResponse{}, err
	}
	if profile.Preferences == nil {
		return textResponse{Text: widgetSettingsNoPrefsText, ReplyMarkup: removeKeyboard()}, nil
	}
	stored := preferenceparser.NormalizePreferences(cloneWidgetPreferences(*profile.Preferences))
	if widgetDistrictsChecksum(stored) != settings.DistrictsChecksum {
		return textResponse{Text: widgetSettingsStaleText, ReplyMarkup: removeKeyboard()}, nil
	}
	next, changes := applyWidgetSettings(stored, settings)
	if len(changes) == 0 {
		return textResponse{Text: widgetSettingsUnchangedText, ReplyMarkup: removeKeyboard()}, nil
	}
	draft := preferenceparser.Draft{Preferences: next, RawQuery: "widget settings"}
	area, err := b.agentSearchArea(ctx, profile, &draft, nil)
	if err != nil {
		return textResponse{Text: friendlyFailure("search area", chatID, err), ReplyMarkup: removeKeyboard()}, nil
	}
	if err := b.saveDraft(ctx, chatID, draft, area); err != nil {
		return textResponse{Text: friendlyFailure("save filters", chatID, err), ReplyMarkup: removeKeyboard()}, nil
	}
	reply := "Сохранил настройки из виджета:\n• " + strings.Join(changes, "\n• ")
	b.recordWidgetSettingsHistory(ctx, chatID, reply)
	return textResponse{Text: reply, ReplyMarkup: removeKeyboard()}, nil
}

func (b *Bot) recordWidgetSettingsHistory(ctx context.Context, chatID int64, reply string) {
	profile, ok, err := b.store.GetProfile(ctx, profiles.IDForTelegramChat(chatID))
	if err != nil || !ok {
		return
	}
	now := time.Now()
	history := append(append([]profiles.MCPMessage{}, profile.MCPHistory...),
		profiles.MCPMessage{Role: "user", Content: "Сохранил настройки в виджете", CreatedAt: now},
		profiles.MCPMessage{Role: "assistant", Content: reply, CreatedAt: now},
	)
	_ = b.store.UpsertProfile(ctx, profiles.Profile{ID: profile.ID, TelegramChatID: chatID, MCPHistory: history})
}

type settingsWidget struct {
	HTML     string
	ChatData string
	Caption  string
}

type settingsWidgetData struct {
	City             string   `json:"c"`
	MaxBudget        int      `json:"b"`
	MinAreaM2        int      `json:"a"`
	MustHave         int      `json:"m"`
	Districts        []string `json:"d"`
	DistrictChecksum string   `json:"v"`
	BotUsername      string   `json:"u"`
}

func (b *Bot) buildSettingsWidget(ctx context.Context, profile profiles.Profile) (*settingsWidget, error) {
	if profile.Preferences == nil {
		return nil, fmt.Errorf("profile has no preferences")
	}
	username, err := b.botUsername(ctx)
	if err != nil {
		return nil, err
	}
	prefs := preferenceparser.NormalizePreferences(cloneWidgetPreferences(*profile.Preferences))
	names := widgetDistrictNames(prefs)
	if len(names) > maxWidgetDistricts {
		names = names[:maxWidgetDistricts]
	}
	for i, name := range names {
		names[i] = shortenedDistrictName(name)
	}
	data := settingsWidgetData{
		City:             b.cityOf(prefs).DisplayName,
		MaxBudget:        min(prefs.MaxBudget, maxWidgetBudget),
		MinAreaM2:        min(widgetAreaValue(prefs.MinAreaM2), maxWidgetAreaM2),
		MustHave:         widgetMustHaveBits(prefs),
		Districts:        names,
		DistrictChecksum: widgetDistrictsChecksum(prefs),
		BotUsername:      username,
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	if len(encoded) > 1024 {
		return nil, fmt.Errorf("widget chat data is %d bytes, limit is 1024", len(encoded))
	}
	return &settingsWidget{HTML: searchSettingsWidgetHTML, ChatData: string(encoded), Caption: settingsWidgetCaption}, nil
}

func widgetFallbackSummary(prefs domain.UserPreferences) string {
	prefs = preferenceparser.NormalizePreferences(cloneWidgetPreferences(prefs))
	lines := []string{
		"Не получилось открыть виджет настроек. Сейчас в поиске:",
		"• Бюджет до: " + budgetLabel(prefs.MaxBudget),
		"• Площадь от: " + areaLabel(prefs.MinAreaM2),
		"• Посудомойка: " + requirementLabel(prefs.RequireDishwasher),
		"• Духовка: " + requirementLabel(prefs.RequireOven),
	}
	if names := widgetDistrictNames(prefs); len(names) > 0 {
		lines = append(lines, "• Районы: "+strings.Join(names, ", "))
	}
	lines = append(lines, "Напиши, что поменять, и я обновлю.")
	return strings.Join(lines, "\n")
}

func (b *Bot) settingsWidgetResponse(ctx context.Context, chatID int64) (textResponse, error) {
	b.deleteSession(chatID)
	profile, err := b.ensureProfile(ctx, chatID)
	if err != nil {
		return textResponse{}, err
	}
	if profile.Preferences == nil {
		return textResponse{Text: widgetSettingsNoPrefsText, ReplyMarkup: removeKeyboard()}, nil
	}
	fallback := widgetFallbackSummary(*profile.Preferences)
	widget, err := b.buildSettingsWidget(ctx, profile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "telegram bot settings widget build failed for chat %d: %v\n", chatID, err)
		return textResponse{Text: fallback, ReplyMarkup: removeKeyboard()}, nil
	}
	return textResponse{Text: fallback, ReplyMarkup: removeKeyboard(), Widget: widget}, nil
}

func (b *Bot) SendSettingsWidget(ctx context.Context, chatID int64) (string, error) {
	response, err := b.settingsWidgetResponse(ctx, chatID)
	if err != nil {
		return "", err
	}
	if response.Widget == nil {
		return "widget is not available, tell the user: " + response.Text, nil
	}
	if err := b.postWidget(ctx, chatID, *response.Widget); err != nil {
		fmt.Fprintf(os.Stderr, "telegram bot settings widget failed for chat %d: %v\n", chatID, err)
		return "widget could not be sent, tell the user: " + response.Text, nil
	}
	return "settings widget sent to the chat", nil
}

func (b *Bot) postWidget(ctx context.Context, chatID int64, widget settingsWidget) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":   chatID,
		"html":      widget.HTML,
		"chat_data": widget.ChatData,
		"caption":   widget.Caption,
	})
	if err != nil {
		return err
	}
	endpoint := b.widgetEndpoint
	if endpoint == "" {
		endpoint = defaultWidgetEndpoint
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf(endpoint, b.token), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	return b.doTelegramJSON(request, nil)
}

func (b *Bot) botUsername(ctx context.Context) (string, error) {
	b.mu.Lock()
	cached := b.username
	b.mu.Unlock()
	if cached != "" {
		return cached, nil
	}
	endpoint := b.getMeEndpoint
	if endpoint == "" {
		endpoint = defaultGetMeEndpoint
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf(endpoint, b.token), nil)
	if err != nil {
		return "", err
	}
	var me struct {
		Username string `json:"username"`
	}
	if err := b.doTelegramJSON(request, &me); err != nil {
		return "", err
	}
	if me.Username == "" {
		return "", fmt.Errorf("telegram getMe returned no username")
	}
	b.mu.Lock()
	b.username = me.Username
	b.mu.Unlock()
	return me.Username, nil
}

func (b *Bot) doTelegramJSON(request *http.Request, result any) error {
	method := path.Base(request.URL.Path)
	response, err := b.client.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("telegram %s: %w", method, urlErr.Err)
		}
		return fmt.Errorf("telegram %s failed", method)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	var envelope struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return fmt.Errorf("telegram %s: status %d", method, response.StatusCode)
	}
	if !envelope.OK {
		return fmt.Errorf("telegram %s: %s", method, envelope.Description)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, result)
}
