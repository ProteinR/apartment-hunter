package telegram

import (
	"strings"
	"testing"

	"apartment-hunter/internal/evaluation"
)

func TestBuildListingRichMessagePutsMapFirstAndCollapsesFullCriteria(t *testing.T) {
	message := BuildListingRichMessage(ListingNotification{
		Report: evaluation.ReportView{
			Title:       "2+1 <Rustaveli> & sea",
			HasScore:    true,
			ScoreTotal:  41,
			Status:      "rejected_ai",
			StatusLabel: "не прошло AI-фильтры",
			Price:       680,
			AreaM2:      61,
			Criteria: []evaluation.ReportCriterion{
				{Label: "Цена", Outcome: evaluation.CriterionMet, Detail: "680 при лимите 700"},
				{Label: "Ремонт", Outcome: evaluation.CriterionFailed, Detail: "обнаружены глянцевые потолки"},
			},
			MessageLink: "https://t.me/batumi_flats/9921",
			SourceLabel: "@batumi_flats",
		},
		MapImage:  []byte("map"),
		Photos:    [][]byte{[]byte("one"), []byte("two")},
		ReplyHint: "Ответь на это сообщение",
	})

	if len(message.Media) != 3 || message.Media[0].ID != "map" || message.Media[1].ID != "photo1" {
		t.Fatalf("expected map first then photos, got %#v", message.Media)
	}
	body := message.HTML
	for _, expected := range []string{
		"<h3>2+1 &lt;Rustaveli&gt; &amp; sea</h3>",
		`<tg-slideshow><img src="tg://photo?id=map"/><img src="tg://photo?id=photo1"/>`,
		"<li>✗ <b>Ремонт</b>: обнаружены глянцевые потолки</li>",
		"<details><summary>Все критерии (2)</summary>",
		"<footer>Ответь на это сообщение</footer>",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("expected %q in:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "Все критерии выполнены") {
		t.Fatalf("failed listing must not claim all criteria are met:\n%s", body)
	}
	if strings.Index(body, "<li>") > strings.Index(body, "<details>") {
		t.Fatalf("failed criteria must appear above the collapsed list:\n%s", body)
	}
}

func TestBuildListingRichMessageSaysAllCriteriaMetForPassedListing(t *testing.T) {
	message := BuildListingRichMessage(ListingNotification{
		Report: evaluation.ReportView{
			Title:    "2+1",
			Status:   "passed",
			Criteria: []evaluation.ReportCriterion{{Label: "Цена", Outcome: evaluation.CriterionMet}},
		},
		MapImage: []byte("map"),
	})
	if !strings.Contains(message.HTML, "<p>✓ Все критерии выполнены</p>") || strings.Contains(message.HTML, "<li>") {
		t.Fatalf("unexpected passed body:\n%s", message.HTML)
	}
	if !strings.Contains(message.HTML, `<img src="tg://photo?id=map"/>`) || strings.Contains(message.HTML, "tg-slideshow") {
		t.Fatalf("single map should be a plain image block:\n%s", message.HTML)
	}
}

func TestBuildListingRichMessageWithoutMediaHasNoGallery(t *testing.T) {
	message := BuildListingRichMessage(ListingNotification{
		Report: evaluation.ReportView{Title: "Студия", Status: "rejected_basic", StatusLabel: "не прошло базовые фильтры",
			Criteria: []evaluation.ReportCriterion{{Label: "Метраж", Outcome: evaluation.CriterionFailed, Detail: "32.0 м2 при минимуме 50.0"}}},
	})
	if len(message.Media) != 0 || strings.Contains(message.HTML, "<img") {
		t.Fatalf("expected no media:\n%s", message.HTML)
	}
}

func TestListingNotificationKeyboardOpensListingAndRejects(t *testing.T) {
	markup := ListingNotificationKeyboard(evaluation.ReportView{MessageLink: "https://t.me/batumi_flats/9921"})
	row := markup.InlineKeyboard[0]
	if len(row) != 2 || row[0].URL != "https://t.me/batumi_flats/9921" || row[0].Style != "primary" || row[1].CallbackData != ListingRejectCallbackData {
		t.Fatalf("unexpected keyboard: %#v", row)
	}
	withoutLink := ListingNotificationKeyboard(evaluation.ReportView{})
	if len(withoutLink.InlineKeyboard[0]) != 1 || withoutLink.InlineKeyboard[0][0].Text != "Не подходит" {
		t.Fatalf("expected only reject button without link: %#v", withoutLink.InlineKeyboard)
	}
}
