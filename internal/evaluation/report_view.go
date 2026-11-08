package evaluation

import (
	"fmt"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/scoring"
)

type CriterionOutcome string

const (
	CriterionMet     CriterionOutcome = "met"
	CriterionFailed  CriterionOutcome = "failed"
	CriterionSkipped CriterionOutcome = "skipped"
)

type ReportCriterion struct {
	Label   string
	Outcome CriterionOutcome
	Detail  string
}

type ReportView struct {
	Title            string
	HasScore         bool
	ScoreTotal       float64
	Status           string
	StatusLabel      string
	Price            int
	AreaM2           float64
	District         string
	Latitude         float64
	Longitude        float64
	Criteria         []ReportCriterion
	RejectionReasons []string
	FailureReason    string
	AIUsageLine      string
	MessageLink      string
	SourceLabel      string
}

func (v ReportView) FailedCriteria() []ReportCriterion {
	var failed []ReportCriterion
	for _, criterion := range v.Criteria {
		if criterion.Outcome == CriterionFailed {
			failed = append(failed, criterion)
		}
	}
	return failed
}

func (v ReportView) Passed() bool {
	return v.Status == "passed"
}

func BuildReportView(record domain.ListingEvaluationRecord, prefs domain.UserPreferences) ReportView {
	prefs = scoring.NormalizeScoringPreferences(prefs)
	view := ReportView{
		Status:        record.Status,
		StatusLabel:   statusLabel(record.Status),
		Price:         record.Listing.Price,
		AreaM2:        record.Listing.AreaM2,
		District:      strings.TrimSpace(record.Listing.District),
		Latitude:      record.Listing.Latitude,
		Longitude:     record.Listing.Longitude,
		FailureReason: record.FailureReason,
		MessageLink:   record.Source.MessageLink,
		SourceLabel:   reportSourceLabel(record.Source),
	}
	if record.Listing.Title != "" && record.Listing.Title != "Telegram listing" {
		view.Title = record.Listing.Title
	}
	if record.Score != nil {
		view.HasScore = true
		view.ScoreTotal = record.Score.ScoreTotal
		for _, reason := range record.Score.RejectionReasons {
			view.RejectionReasons = append(view.RejectionReasons, rejectionReasonLabel(reason))
		}
	}
	for _, item := range checklistItems(record, prefs) {
		view.Criteria = append(view.Criteria, ReportCriterion{
			Label:   item.Label,
			Outcome: criterionOutcome(item.Mark),
			Detail:  criterionDetail(item.Note),
		})
	}
	view.Criteria = withRejectionDetails(view.Criteria, record)
	if record.AIUsage != nil && record.AIUsage.TotalTokens > 0 {
		view.AIUsageLine = fmt.Sprintf("%s %s · %d токенов · $%.6f", aiProviderLabel(record.AIUsage.Provider), record.AIUsage.Model, record.AIUsage.TotalTokens, record.AIUsage.CostUSD)
	}
	return view
}

func reportSourceLabel(source domain.ListingSourceRef) string {
	if username := strings.TrimPrefix(strings.TrimSpace(source.ChannelUsername), "@"); username != "" {
		return "@" + username
	}
	return strings.TrimSpace(source.ChannelName)
}

func criterionOutcome(mark string) CriterionOutcome {
	switch mark {
	case "✅":
		return CriterionMet
	case "❌":
		return CriterionFailed
	default:
		return CriterionSkipped
	}
}

func criterionDetail(note string) string {
	note = strings.TrimSpace(note)
	note = strings.TrimPrefix(note, "(")
	note = strings.TrimSuffix(note, ")")
	return strings.TrimSpace(note)
}

func withRejectionDetails(criteria []ReportCriterion, record domain.ListingEvaluationRecord) []ReportCriterion {
	if record.Score == nil {
		return criteria
	}
	var interiorReasons, viewReasons []string
	for _, reason := range record.Score.RejectionReasons {
		lower := strings.ToLower(reason)
		switch {
		case strings.Contains(lower, "glossy ceilings"), strings.Contains(lower, "dark walls"), strings.Contains(lower, "old renovation"):
			interiorReasons = append(interiorReasons, rejectionReasonLabel(reason))
		case strings.Contains(lower, "windows facing"):
			viewReasons = append(viewReasons, rejectionReasonLabel(reason))
		}
	}
	for i := range criteria {
		if criteria[i].Outcome != CriterionFailed {
			continue
		}
		switch criteria[i].Label {
		case "Ремонт":
			criteria[i].Detail = joinDetails(interiorReasons, criteria[i].Detail)
		case "Вид из окна":
			criteria[i].Detail = joinDetails(viewReasons, criteria[i].Detail)
		}
	}
	return criteria
}

func joinDetails(reasons []string, existing string) string {
	parts := append([]string{}, reasons...)
	if existing != "" {
		parts = append(parts, existing)
	}
	return strings.Join(parts, ", ")
}
