package telegram

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"apartment-hunter/internal/evaluation"
)

func solidJPEG(t *testing.T, width, height int, fill color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, fill)
		}
	}
	var buffer bytes.Buffer
	if err := jpeg.Encode(&buffer, img, nil); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func solidPNG(t *testing.T, width, height int, fill color.RGBA) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for x := 0; x < width; x++ {
		for y := 0; y < height; y++ {
			img.Set(x, y, fill)
		}
	}
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

var (
	collageRed   = color.RGBA{220, 40, 40, 255}
	collageGreen = color.RGBA{40, 200, 60, 255}
	collageBlue  = color.RGBA{40, 60, 220, 255}
	collageGray  = color.RGBA{128, 128, 128, 255}
)

func decodeCollage(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != listingCollageWidth || img.Bounds().Dy() != listingCollageHeight {
		t.Fatalf("collage size %v", img.Bounds())
	}
	return img
}

func assertCollageColor(t *testing.T, img image.Image, x, y int, want color.RGBA) {
	t.Helper()
	r, g, b, _ := img.At(x, y).RGBA()
	near := func(got uint32, want uint8) bool {
		diff := int(got>>8) - int(want)
		return diff > -40 && diff < 40
	}
	if !near(r, want.R) || !near(g, want.G) || !near(b, want.B) {
		t.Fatalf("pixel %d,%d = %d,%d,%d want %v", x, y, r>>8, g>>8, b>>8, want)
	}
}

func TestListingCollagePutsMainPhotoLeftAndSidePhotosThenMapRight(t *testing.T) {
	photos := [][]byte{
		solidJPEG(t, 1280, 960, collageRed),
		[]byte("broken"),
		solidJPEG(t, 600, 900, collageGreen),
		solidJPEG(t, 900, 600, collageBlue),
		solidJPEG(t, 900, 600, collageRed),
	}
	collage, err := BuildListingCollage(photos, solidPNG(t, 1200, 800, collageGray))
	if err != nil {
		t.Fatal(err)
	}
	img := decodeCollage(t, collage)
	assertCollageColor(t, img, 400, 400, collageRed)
	assertCollageColor(t, img, 1000, 130, collageGreen)
	assertCollageColor(t, img, 1000, 400, collageBlue)
	assertCollageColor(t, img, 1000, 670, collageGray)
}

func TestListingCollageWithoutPhotosUsesMapAndWithoutAnythingFails(t *testing.T) {
	collage, err := BuildListingCollage(nil, solidPNG(t, 1200, 800, collageGray))
	if err != nil {
		t.Fatal(err)
	}
	assertCollageColor(t, decodeCollage(t, collage), 300, 300, collageGray)
	single, err := BuildListingCollage([][]byte{solidJPEG(t, 800, 600, collageRed)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCollageColor(t, decodeCollage(t, single), 1100, 700, collageRed)
	if _, err := BuildListingCollage([][]byte{[]byte("x")}, nil); !errors.Is(err, errNoCollageImages) {
		t.Fatalf("err = %v", err)
	}
}

func captionReport() evaluation.ReportView {
	return evaluation.ReportView{
		Title:       "#сдам Сдается квартира <b>у моря</b> & рядом парк #аренда",
		HasScore:    true,
		ScoreTotal:  70,
		Status:      "passed",
		StatusLabel: "подходит",
		Price:       650,
		AreaM2:      50,
		Criteria: []evaluation.ReportCriterion{
			{Label: "Цена", Outcome: evaluation.CriterionMet, Detail: "$650"},
			{Label: "Ремонт", Outcome: evaluation.CriterionFailed, Detail: "темные стены"},
			{Label: "Животные", Outcome: evaluation.CriterionSkipped},
		},
		MessageLink: "https://t.me/domikibatumi/1144629",
	}
}

func TestListingCaptionIsCompactEscapedAndHasExpandableCriteria(t *testing.T) {
	caption := BuildListingCaption(captionReport(), "hint")
	for _, want := range []string{
		"<b>$650 / мес</b> · 50 м² · 70/100 · подходит",
		"Сдается квартира &lt;b&gt;у моря&lt;/b&gt; &amp; рядом парк",
		"✗ Ремонт: темные стены",
		"<blockquote expandable>Критерии\n✓ Цена — $650\n✗ Ремонт — темные стены\n– Животные</blockquote>",
		"<i>hint</i>",
	} {
		if !strings.Contains(caption, want) {
			t.Fatalf("caption missing %q:\n%s", want, caption)
		}
	}
}

func TestListingCaptionDropsCriteriaToFitTelegramLimit(t *testing.T) {
	report := captionReport()
	report.Criteria = nil
	for i := 0; i < 40; i++ {
		report.Criteria = append(report.Criteria, evaluation.ReportCriterion{Label: "Очень длинный критерий номер", Outcome: evaluation.CriterionMet, Detail: strings.Repeat("подробности ", 6)})
	}
	caption := BuildListingCaption(report, "hint")
	if captionUnits(caption) > maxListingCaptionUnits {
		t.Fatalf("caption units %d", captionUnits(caption))
	}
	if !strings.Contains(caption, "и еще ") || !strings.HasSuffix(strings.TrimSpace(caption), "<i>hint</i>") {
		t.Fatalf("caption tail:\n%s", caption)
	}
	if strings.Count(caption, "<blockquote") != strings.Count(caption, "</blockquote>") {
		t.Fatalf("unbalanced blockquote")
	}
}

type fakeCollageTelegram struct {
	method    string
	fields    map[string]string
	photoSize int
	status    int
	reply     string
}

func (f *fakeCollageTelegram) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.method = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		f.fields = map[string]string{}
		_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		reader := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := reader.NextPart()
			if err != nil {
				break
			}
			value, _ := io.ReadAll(part)
			if part.FileName() != "" {
				f.photoSize = len(value)
				continue
			}
			f.fields[part.FormName()] = string(value)
		}
		if f.status != 0 {
			w.WriteHeader(f.status)
		}
		_, _ = io.WriteString(w, f.reply)
	}))
}

func TestSendListingCollageSendsOnePhotoWithCaptionAndButtons(t *testing.T) {
	fake := &fakeCollageTelegram{reply: `{"ok":true,"result":{"message_id":77,"chat":{"id":42}}}`}
	server := fake.server(t)
	defer server.Close()
	id, err := sendListingCollageNotification(context.Background(), "TOKEN", 42, ListingCollageNotification{
		Report:   captionReport(),
		MapImage: solidPNG(t, 600, 400, collageGray),
		Photos:   [][]byte{solidJPEG(t, 800, 600, collageRed)},
		Hint:     "hint",
	}, listingCollageEndpoints{apiServerURL: server.URL, client: server.Client()})
	if err != nil || id != 77 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if fake.method != "sendPhoto" || fake.photoSize == 0 || fake.fields["parse_mode"] != "HTML" {
		t.Fatalf("request %s fields=%v size=%d", fake.method, fake.fields, fake.photoSize)
	}
	if !strings.Contains(fake.fields["caption"], "<blockquote expandable>") {
		t.Fatalf("caption = %q", fake.fields["caption"])
	}
	if !strings.Contains(fake.fields["reply_markup"], ListingRejectCallbackData) || !strings.Contains(fake.fields["reply_markup"], "1144629") {
		t.Fatalf("markup = %q", fake.fields["reply_markup"])
	}
}

func TestSendListingCollageWithoutImagesSendsTextAndReportsRejection(t *testing.T) {
	fake := &fakeCollageTelegram{status: http.StatusBadRequest, reply: `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`}
	server := fake.server(t)
	defer server.Close()
	_, err := sendListingCollageNotification(context.Background(), "SECRET123", 42, ListingCollageNotification{Report: captionReport()}, listingCollageEndpoints{apiServerURL: server.URL, client: server.Client()})
	if fake.method != "sendMessage" {
		t.Fatalf("method = %s", fake.method)
	}
	if !errors.Is(err, ErrListingCollageRejected) || strings.Contains(err.Error(), "SECRET123") {
		t.Fatalf("err = %v", err)
	}
}

func TestListingCaptionSkipsHashtagOnlyTitle(t *testing.T) {
	report := captionReport()
	report.Title = "#сдам #1спальня #аренда"
	if caption := BuildListingCaption(report, ""); strings.Contains(caption, "#") {
		t.Fatalf("caption kept hashtags:\n%s", caption)
	}
}

func TestSendListingCollageNetworkErrorHidesTokenAndKeepsCause(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	server.Close()
	_, err := sendListingCollageNotification(context.Background(), "SECRET123", 42, ListingCollageNotification{Report: captionReport()}, listingCollageEndpoints{apiServerURL: server.URL, client: server.Client()})
	if err == nil || strings.Contains(err.Error(), "SECRET123") || errors.Is(err, ErrListingCollageRejected) {
		t.Fatalf("err = %v", err)
	}
	var netErr interface{ Timeout() bool }
	if !errors.As(err, &netErr) {
		t.Fatalf("cause lost: %v", err)
	}
}

func TestRedactTokenErrorKeepsErrorChain(t *testing.T) {
	cause := context.Canceled
	err := redactTokenError(errors.Join(errors.New("POST https://api.telegram.org/botSECRET123/sendMessage"), cause), "SECRET123")
	if strings.Contains(err.Error(), "SECRET123") || !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if redactTokenError(nil, "x") != nil {
		t.Fatalf("nil error wrapped")
	}
}

func TestRedactAnyBotTokenHidesTokenInURL(t *testing.T) {
	got := redactAnyBotToken(`error do request for method sendMessage, Post "https://api.telegram.org/bot123456:AAH-x_Y9/sendMessage": EOF`)
	if strings.Contains(got, "AAH-x_Y9") || !strings.Contains(got, "/bot<token>/sendMessage") {
		t.Fatalf("got %s", got)
	}
}
