package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"apartment-hunter/internal/profiles"
)

const (
	quietSearchThreshold     = 24 * time.Hour
	quietSearchMaxAge        = 7 * 24 * time.Hour
	quietSearchCheckInterval = 30 * time.Minute
	nudgeEarliestLocalHour   = 10
	nudgeLatestLocalHour     = 21
)

var nudgeLocalZone = time.FixedZone("Asia/Tbilisi", 4*60*60)

type quietSearchStore interface {
	QuietSearches(ctx context.Context, quietFor, maxQuiet time.Duration) ([]profiles.QuietSearch, error)
	MarkQuietNudgeSent(ctx context.Context, profileID string, quietSince time.Time) error
}

type quietSearchNudger struct {
	store         quietSearchStore
	send          func(ctx context.Context, chatID int64, text string) error
	onlyProfileID string
	errOut        io.Writer
	now           func() time.Time
}

func (n quietSearchNudger) Run(ctx context.Context, interval time.Duration) {
	n.NudgeQuietSearches(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n.NudgeQuietSearches(ctx)
		}
	}
}

func (n quietSearchNudger) NudgeQuietSearches(ctx context.Context) {
	hour := n.currentTime().In(nudgeLocalZone).Hour()
	if hour < nudgeEarliestLocalHour || hour >= nudgeLatestLocalHour {
		return
	}
	quiet, err := n.store.QuietSearches(ctx, quietSearchThreshold, quietSearchMaxAge)
	if err != nil {
		n.logError("quiet search check failed: %v", err)
		return
	}
	for _, search := range quiet {
		if n.onlyProfileID != "" && search.ProfileID != n.onlyProfileID {
			continue
		}
		if err := n.send(ctx, search.TelegramChatID, quietSearchMessage(search.StatusCounts)); err != nil {
			n.logError("quiet search nudge failed for %s: %v", search.ProfileID, err)
		}
		if err := n.store.MarkQuietNudgeSent(ctx, search.ProfileID, search.QuietSince); err != nil {
			n.logError("quiet search nudge mark failed for %s: %v", search.ProfileID, err)
		}
	}
}

func quietSearchMessage(counts map[string]int) string {
	reasons := []struct {
		status string
		label  string
	}{
		{"rejected_basic", "цена, площадь или обязательные удобства"},
		{"rejected_location", "вне зоны поиска"},
		{"rejected_ai", "ремонт или вид по фото"},
		{"duplicate", "повторы уже виденных"},
	}
	checked := 0
	for _, reason := range reasons {
		checked += counts[reason.status]
	}
	if checked == 0 {
		return "Уже больше суток в подключенных источниках не было новых объявлений об аренде, поэтому присылать нечего. Можно подключить еще каналы: пришли @username или ссылку t.me, или попроси «предложи каналы»."
	}
	lines := []string{fmt.Sprintf("Уже больше суток ни одно объявление не подошло: за это время я проверил %d.", checked)}
	var reasonLines []string
	for _, reason := range reasons {
		if counts[reason.status] > 0 {
			reasonLines = append(reasonLines, fmt.Sprintf("- %s: %d", reason.label, counts[reason.status]))
		}
	}
	lines = append(lines, "", "Отсеялись так:")
	lines = append(lines, reasonLines...)
	lines = append(lines, "", "Если хочешь больше вариантов, ослабь критерии: например, «подними бюджет до 800$» или «расширь район». А «присылай все отчеты» покажет каждое объявление с причиной отказа.")
	return strings.Join(lines, "\n")
}

func (n quietSearchNudger) currentTime() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

func (n quietSearchNudger) logError(format string, args ...any) {
	if n.errOut != nil {
		fmt.Fprintf(n.errOut, format+"\n", args...)
	}
}
