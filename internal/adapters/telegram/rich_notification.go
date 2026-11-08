package telegram

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"apartment-hunter/internal/evaluation"

	telegrambot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

type ListingNotification struct {
	Report    evaluation.ReportView
	MapImage  []byte
	Photos    [][]byte
	ReplyHint string
}

var ErrRichMessageRejected = errors.New("telegram rejected the rich message")

func SendListingNotificationMessageID(ctx context.Context, token string, chatID int64, notification ListingNotification) (int, error) {
	api, err := telegrambot.New(token, telegrambot.WithHTTPClient(60*time.Second, &http.Client{Timeout: 60 * time.Second}))
	if err != nil {
		return 0, redactTokenError(err, token)
	}
	params := &telegrambot.SendRichMessageParams{
		ChatID:      chatID,
		RichMessage: BuildListingRichMessage(notification),
	}
	if markup := ListingNotificationKeyboard(notification.Report); markup != nil {
		params.ReplyMarkup = markup
	}
	message, err := api.SendRichMessage(ctx, params)
	if err != nil {
		if errors.Is(err, telegrambot.ErrorBadRequest) {
			return 0, fmt.Errorf("%w: %s", ErrRichMessageRejected, redactBotToken(err.Error(), token))
		}
		return 0, redactTokenError(err, token)
	}
	return message.ID, nil
}

func BuildListingRichMessage(notification ListingNotification) models.InputRichMessage {
	media := listingNotificationMedia(notification)
	var body strings.Builder
	report := notification.Report
	writeListingHeader(&body, report)
	writeListingGallery(&body, media)
	writeListingFacts(&body, report)
	writeListingVerdict(&body, report)
	writeListingCriteria(&body, report)
	writeListingEvaluationDetails(&body, report)
	if hint := strings.TrimSpace(notification.ReplyHint); hint != "" {
		body.WriteString("<footer>" + html.EscapeString(hint) + "</footer>")
	}
	return models.InputRichMessage{
		HTML:                body.String(),
		Media:               media,
		SkipEntityDetection: true,
	}
}

func listingNotificationMedia(notification ListingNotification) []models.InputRichMessageMedia {
	var media []models.InputRichMessageMedia
	if len(notification.MapImage) > 0 {
		media = append(media, richPhotoMedia("map", notification.MapImage))
	}
	for i, photo := range notification.Photos {
		if len(photo) == 0 {
			continue
		}
		media = append(media, richPhotoMedia(fmt.Sprintf("photo%d", i+1), photo))
	}
	return media
}

func richPhotoMedia(id string, data []byte) models.InputRichMessageMedia {
	return models.InputRichMessageMedia{
		ID: id,
		Media: &models.InputMediaPhoto{
			Media:           "attach://" + id,
			MediaAttachment: bytes.NewReader(data),
		},
	}
}

func writeListingHeader(body *strings.Builder, report evaluation.ReportView) {
	title := strings.TrimSpace(report.Title)
	if title == "" {
		title = "Объявление о квартире"
	}
	body.WriteString("<h3>" + html.EscapeString(title) + "</h3>")
	if report.HasScore {
		body.WriteString(fmt.Sprintf("<p><b>%.0f</b> из 100 · %s</p>", report.ScoreTotal, html.EscapeString(verdictLabel(report))))
	}
}

func verdictLabel(report evaluation.ReportView) string {
	if report.Passed() {
		return "подходит"
	}
	return report.StatusLabel
}

func writeListingGallery(body *strings.Builder, media []models.InputRichMessageMedia) {
	if len(media) == 0 {
		return
	}
	if len(media) == 1 {
		body.WriteString(`<img src="tg://photo?id=` + media[0].ID + `"/>`)
		return
	}
	body.WriteString("<tg-slideshow>")
	for _, item := range media {
		body.WriteString(`<img src="tg://photo?id=` + item.ID + `"/>`)
	}
	body.WriteString("</tg-slideshow>")
}

func writeListingFacts(body *strings.Builder, report evaluation.ReportView) {
	var facts []string
	if report.Price > 0 {
		facts = append(facts, fmt.Sprintf("<b>$%d</b> в месяц", report.Price))
	}
	if report.AreaM2 > 0 {
		facts = append(facts, formatArea(report.AreaM2)+" м²")
	}
	if len(facts) > 0 {
		body.WriteString("<p>" + strings.Join(facts, " · ") + "</p>")
	}
}

func formatArea(area float64) string {
	if area == float64(int(area)) {
		return fmt.Sprintf("%d", int(area))
	}
	return fmt.Sprintf("%.1f", area)
}

func writeListingVerdict(body *strings.Builder, report evaluation.ReportView) {
	failed := report.FailedCriteria()
	if len(failed) > 0 {
		body.WriteString("<ul>")
		for _, criterion := range failed {
			line := "✗ <b>" + html.EscapeString(criterion.Label) + "</b>"
			if criterion.Detail != "" {
				line += ": " + html.EscapeString(criterion.Detail)
			}
			body.WriteString("<li>" + line + "</li>")
		}
		body.WriteString("</ul>")
	} else if report.Passed() {
		body.WriteString("<p>✓ Все критерии выполнены</p>")
	} else {
		body.WriteString("<p>Статус: " + html.EscapeString(report.StatusLabel) + "</p>")
	}
	if report.FailureReason != "" {
		body.WriteString("<p>⚠️ Ошибка: " + html.EscapeString(report.FailureReason) + "</p>")
	}
}

func writeListingCriteria(body *strings.Builder, report evaluation.ReportView) {
	if len(report.Criteria) == 0 {
		return
	}
	body.WriteString(fmt.Sprintf("<details><summary>Все критерии (%d)</summary><table>", len(report.Criteria)))
	for _, criterion := range report.Criteria {
		body.WriteString("<tr><td>" + html.EscapeString(criterion.Label) + "</td><td>" + criterionMark(criterion.Outcome) + "</td><td>" + html.EscapeString(criterion.Detail) + "</td></tr>")
	}
	body.WriteString("</table></details>")
}

func criterionMark(outcome evaluation.CriterionOutcome) string {
	switch outcome {
	case evaluation.CriterionMet:
		return "✓"
	case evaluation.CriterionFailed:
		return "✗"
	default:
		return "–"
	}
}

func writeListingEvaluationDetails(body *strings.Builder, report evaluation.ReportView) {
	if report.AIUsageLine == "" {
		return
	}
	body.WriteString("<details><summary>Как я оценивал</summary><p>" + html.EscapeString(report.AIUsageLine) + "</p></details>")
}

const (
	ListingRejectCallbackData = "listing:reject"
	listingRejectFeedbackText = "Это объявление мне не подходит"
)

func ListingNotificationKeyboard(report evaluation.ReportView) *models.InlineKeyboardMarkup {
	var row []models.InlineKeyboardButton
	if link := strings.TrimSpace(report.MessageLink); link != "" {
		row = append(row, models.InlineKeyboardButton{Text: "Открыть объявление", URL: link, Style: "primary"})
	}
	row = append(row, models.InlineKeyboardButton{Text: "Не подходит", CallbackData: ListingRejectCallbackData})
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{row}}
}

const (
	listingRejectAlreadyText  = "Уже учел, что это объявление не подходит. Если хочешь объяснить подробнее, ответь на уведомление сообщением."
	listingRejectNotFoundText = "Не нашел это объявление в истории. Напиши в ответ на уведомление, что не подошло."
)

func (b *Bot) handleListingRejectCallback(ctx context.Context, api *telegrambot.Bot, query *models.CallbackQuery) {
	notification := query.Message.Message
	if notification == nil || notification.Chat.ID != query.From.ID {
		answerCallback(ctx, api, query.ID, "")
		return
	}
	if !b.authorizeMessage(ctx, &models.Message{Chat: notification.Chat, From: &query.From}) {
		answerCallback(ctx, api, query.ID, "Нет доступа к боту.")
		return
	}
	answerCallback(ctx, api, query.ID, "")
	chatID := notification.Chat.ID
	chatLock := b.chatLock(chatID)
	chatLock.Lock()
	defer chatLock.Unlock()
	if api != nil {
		sendTyping(ctx, api, chatID)
	}
	reply, rejected, err := b.rejectListingFromButton(ctx, chatID, notification.ID)
	if api == nil {
		return
	}
	if err != nil {
		_ = sendMessage(ctx, api, chatID, friendlyFailure("listing reject button", chatID, err))
		return
	}
	if rejected {
		removeListingRejectButton(ctx, api, notification)
	}
	_ = sendMessageWithReply(ctx, api, chatID, notification.ID, reply)
}

func (b *Bot) rejectListingFromButton(ctx context.Context, chatID int64, notificationMessageID int) (string, bool, error) {
	if b.store == nil {
		return "", false, fmt.Errorf("profile store is required")
	}
	exists, err := b.store.ListingFeedbackExists(ctx, chatID, notificationMessageID)
	if err != nil {
		return "", false, err
	}
	if exists {
		return listingRejectAlreadyText, true, nil
	}
	reply, handled, err := b.saveListingFeedback(ctx, chatID, notificationMessageID, notificationMessageID, listingRejectFeedbackText)
	if err != nil {
		return "", false, err
	}
	if !handled {
		return listingRejectNotFoundText, false, nil
	}
	return reply, true, nil
}

func removeListingRejectButton(ctx context.Context, api *telegrambot.Bot, notification *models.Message) {
	var row []models.InlineKeyboardButton
	if notification.ReplyMarkup != nil {
		for _, buttons := range notification.ReplyMarkup.InlineKeyboard {
			for _, button := range buttons {
				if button.CallbackData != ListingRejectCallbackData {
					row = append(row, button)
				}
			}
		}
	}
	markup := &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
	if len(row) > 0 {
		markup.InlineKeyboard = [][]models.InlineKeyboardButton{row}
	}
	_, _ = api.EditMessageReplyMarkup(ctx, &telegrambot.EditMessageReplyMarkupParams{ChatID: notification.Chat.ID, MessageID: notification.ID, ReplyMarkup: markup})
}

func answerCallback(ctx context.Context, api *telegrambot.Bot, queryID string, text string) {
	if api == nil {
		return
	}
	_, _ = api.AnswerCallbackQuery(ctx, &telegrambot.AnswerCallbackQueryParams{CallbackQueryID: queryID, Text: text})
}
