package listingparser

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/geocoding"
	"apartment-hunter/internal/sources"
)

var (
	priceSeparator             = `[ \t:=\-–—\p{So}\p{Sk}\x{FE0F}\x{200D}]*`
	trailingCurrencySeparator  = `[ \t\x{FE0F}]*`
	priceNumber                = `([0-9]{1,3}(?:[ .,][0-9]{3})+|[0-9]+)`
	currencyFirstPricePattern  = regexp.MustCompile(`(?i)(?:[$₾]|\x{1F4B2}|usd|gel|lari)` + priceSeparator + priceNumber)
	numberFirstPricePattern    = regexp.MustCompile(`(?i)` + priceNumber + trailingCurrencySeparator + `(?:[$₾]|\x{1F4B2}|usd|gel|lari|лари|доллар\p{L}*|долл?(?:[^\p{L}]|$))`)
	priceKeywordPattern        = regexp.MustCompile(`(?i)(?:цена|стоимость|price|\x{1F4B0}|\x{1F4B5}|\x{1F4B8})` + priceSeparator + priceNumber)
	structureAfterPricePattern = regexp.MustCompile(`(?i)^[ \t]*(?:этаж|мес|мин|комнат|спальн|bedroom|[+/][0-9])`)
	unitAfterPricePattern      = regexp.MustCompile(`(?i)^[ \t]*(?:км|km|метр\p{L}*|м\.|м|m)(?:[^\p{L}]|$)`)
	percentAfterPricePattern   = regexp.MustCompile(`^[ \t]*%`)
	usdEquivalentGapPattern    = regexp.MustCompile(`(?i)^[ \t]*(?:[(/≈~=][ \t(/≈~=]*(?:(?:или|около|ок\.?|примерно|approx\.?)[ \t(/≈~=]*)?|(?:или|около|ок\.?|примерно|approx\.?)[ \t(/≈~=]*)$`)
	usdPaymentGapPattern       = regexp.MustCompile(`(?i)^[^\n]*(?:в долларах|in dollars|in usd)[ \t:—–-]*$`)
	rentLabelPattern           = regexp.MustCompile(`(?i)(?:цена|price|стоимость|аренд|rent|\x{1F4B0}|\x{1F4B5}|\x{1F4B8})`)
	extraCostLinePattern       = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:коммунал|интернет|парковк|стоянк|уборк|cleaning|utilit|parking|налог|обслуживан|мусор|квартплат|осмд|тсж|жкх|свет(?:[^\p{L}]|$)|газ(?:[^\p{L}]|$)|вод[аыу](?:[^\p{L}]|$))`)
	dailyPricePattern          = regexp.MustCompile(`(?i)(?:сут(?:к|ок)|посуточ|в день|daily|per night|/ночь|за ночь)`)
	costModifierPattern        = regexp.MustCompile(`(?i)(?:^|[^\p{L}])(?:с|со|без|включая|кроме|with|without|including|excluding)[ \t]+(?:уч[её]та[ \t]+)?\p{L}+`)
	minGELPerUSD               = 2.3
	maxGELPerUSD               = 3.2
	yearAfterPricePattern      = regexp.MustCompile(`(?i)^[ \t]*(?:год\p{L}*|г\.)`)
	floorBeforeNumberPattern   = regexp.MustCompile(`(?i)(?:^|[^0-9 \t])[ \t]*(?:этаж\p{L}*|floor)[ \t:]*$`)
	digitGroupBeforePattern    = regexp.MustCompile(`[0-9][ \t.,]+$`)
	maxPriceValue              = 999999
	unitBeforeNumberPattern    = regexp.MustCompile(`(?i)[мm]$`)
	phoneTailAfterPricePattern = regexp.MustCompile(`^[ \t-]+[0-9]{2}[ \t-]+[0-9]{2}`)
	minKeywordOnlyPrice        = 100
	areaPattern                = regexp.MustCompile(`(?i)([0-9]+(?:[.,][0-9]+)?)\s*(?:m2|m²|м2|м²|кв\.?\s?м\.?|м\.?\s?кв\.?|sq\.?\s?m\.?|²)`)
)

func FromChannelMessage(message sources.ChannelMessage) domain.ApartmentListing {
	text := strings.TrimSpace(message.Text)
	title := firstNonEmptyLine(text)
	if title == "" {
		title = "Telegram listing"
	}

	photos := make([]domain.ApartmentPhoto, 0, len(message.Photos))
	for _, photo := range message.Photos {
		photos = append(photos, domain.ApartmentPhoto{
			URL:     photo.ID,
			Caption: photo.Caption,
		})
	}

	listing := domain.ApartmentListing{
		ID:          listingID(message),
		Title:       title,
		Description: text,
		Price:       parsePrice(text),
		AreaM2:      parseArea(text),
		Photos:      photos,
	}
	lowerText := strings.ToLower(text)
	listing.HasDishwasher = containsAny(lowerText, "dishwasher", "посудомойка", "посудомоечная", "посудомоечную")
	listing.HasOven = containsAny(lowerText, "oven", "духовка", "духовой шкаф", "духовым шкафом")
	if lat, lng, ok := geocoding.FindCoordinates(text); ok {
		listing.Latitude = lat
		listing.Longitude = lng
	}
	return listing
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func listingID(message sources.ChannelMessage) string {
	if message.ChannelID != "" && message.MessageID != 0 {
		return message.ChannelID + ":" + strconv.Itoa(message.MessageID)
	}
	hash := sha256.Sum256([]byte(message.Text))
	return "telegram:" + hex.EncodeToString(hash[:8])
}

func firstNonEmptyLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}

const (
	currencyUSD  = "usd"
	currencyGEL  = "gel"
	currencyNone = ""
)

type priceCandidate struct {
	matchStart  int
	matchEnd    int
	numberStart int
	value       int
	currency    string
}

func parsePrice(text string) int {
	candidates := priceCandidates(text)
	var eligible []priceCandidate
	for _, candidate := range candidates {
		if !isDepositContext(text, candidate.numberStart) {
			eligible = append(eligible, candidate)
		}
	}
	if len(eligible) == 0 {
		return depositOnlyPrice(candidates)
	}
	rentCandidates := filterCandidates(eligible, func(candidate priceCandidate) bool {
		clause := clauseAt(text, candidate.numberStart)
		return !isDailyPriceClause(clause) && !isExtraCostClause(clause)
	})
	if len(rentCandidates) == 0 {
		rentCandidates = filterCandidates(eligible, func(candidate priceCandidate) bool {
			return !isDailyPriceClause(clauseAt(text, candidate.numberStart))
		})
	}
	if len(rentCandidates) == 0 {
		rentCandidates = eligible
	}
	chosen := bestRentCandidate(text, rentCandidates)
	return usdEquivalentOf(text, chosen, eligible).value
}

func filterCandidates(candidates []priceCandidate, keep func(priceCandidate) bool) []priceCandidate {
	var kept []priceCandidate
	for _, candidate := range candidates {
		if keep(candidate) {
			kept = append(kept, candidate)
		}
	}
	return kept
}

func isDailyPriceClause(clause string) bool {
	return dailyPricePattern.MatchString(clause)
}

func isExtraCostClause(clause string) bool {
	return extraCostLinePattern.MatchString(costModifierPattern.ReplaceAllString(clause, " "))
}

func clauseAt(text string, position int) string {
	line := lineAt(text, position)
	lineStart := strings.LastIndex(text[:position], "\n") + 1
	offset := position - lineStart
	clauseStart := strings.LastIndexAny(line[:offset], ",;+()") + 1
	clauseEnd := strings.IndexAny(line[offset:], ",;+()")
	if clauseEnd == -1 {
		return line[clauseStart:]
	}
	return line[clauseStart : offset+clauseEnd]
}

func depositOnlyPrice(candidates []priceCandidate) int {
	for _, candidate := range candidates {
		if candidate.currency != currencyNone {
			return candidate.value
		}
	}
	if len(candidates) > 0 {
		return candidates[0].value
	}
	return 0
}

func bestRentCandidate(text string, candidates []priceCandidate) priceCandidate {
	best := candidates[0]
	bestRank := rentCandidateRank(text, best)
	for _, candidate := range candidates[1:] {
		if rank := rentCandidateRank(text, candidate); rank < bestRank {
			best, bestRank = candidate, rank
		}
	}
	return best
}

func rentCandidateRank(text string, candidate priceCandidate) int {
	switch {
	case candidate.currency != currencyNone && rentLabelPattern.MatchString(lineAt(text, candidate.numberStart)):
		return 0
	case candidate.currency != currencyNone:
		return 1
	default:
		return 2
	}
}

func usdEquivalentOf(text string, chosen priceCandidate, candidates []priceCandidate) priceCandidate {
	if chosen.currency != currencyGEL {
		return chosen
	}
	for index, candidate := range candidates {
		if candidate.numberStart <= chosen.numberStart || candidate.currency != currencyUSD {
			continue
		}
		if isPlausibleUSDEquivalent(chosen.value, candidate.value) || isAdjacentUSDEquivalent(text, chosen, candidate, candidates[:index]) {
			return candidate
		}
		return chosen
	}
	return chosen
}

func isPlausibleUSDEquivalent(gelValue, usdValue int) bool {
	ratio := float64(gelValue) / float64(usdValue)
	return ratio >= minGELPerUSD && ratio <= maxGELPerUSD
}

func isAdjacentUSDEquivalent(text string, chosen, usd priceCandidate, earlier []priceCandidate) bool {
	if usd.matchStart < chosen.matchEnd {
		return false
	}
	for _, candidate := range earlier {
		if candidate.numberStart > chosen.numberStart {
			return false
		}
	}
	gap := text[chosen.matchEnd:usd.matchStart]
	return usdEquivalentGapPattern.MatchString(gap) || usdPaymentGapPattern.MatchString(gap)
}

func lineAt(text string, position int) string {
	lineStart := strings.LastIndex(text[:position], "\n") + 1
	lineEnd := strings.Index(text[position:], "\n")
	if lineEnd == -1 {
		return text[lineStart:]
	}
	return text[lineStart : position+lineEnd]
}

func priceCandidates(text string) []priceCandidate {
	numberFirst := findPriceMatches(text, numberFirstPricePattern, acceptNumberFirstPrice)
	currencyFirst := findPriceMatches(text, currencyFirstPricePattern, acceptCurrencyFirstPrice)
	keywordOnly := findPriceMatches(text, priceKeywordPattern, acceptKeywordOnlyPrice)

	var candidates []priceCandidate
	for _, match := range numberFirst {
		candidates = append(candidates, match.candidate())
	}
	for _, match := range currencyFirst {
		if !currencyClaimedByNumberFirst(match, numberFirst) {
			candidates = append(candidates, match.candidate())
		}
	}
	for _, match := range keywordOnly {
		candidates = append(candidates, match.candidate())
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].numberStart < candidates[j].numberStart
	})
	unique := candidates[:0]
	for _, candidate := range candidates {
		if len(unique) > 0 && unique[len(unique)-1].numberStart == candidate.numberStart {
			continue
		}
		unique = append(unique, candidate)
	}
	return unique
}

type priceMatch struct {
	text        string
	matchStart  int
	matchEnd    int
	numberStart int
	numberEnd   int
	value       int
}

func (match priceMatch) candidate() priceCandidate {
	return priceCandidate{matchStart: match.matchStart, matchEnd: match.matchEnd, numberStart: match.numberStart, value: match.value, currency: match.currency()}
}

func (match priceMatch) currency() string {
	marker := strings.ToLower(match.text[match.matchStart:match.numberStart] + match.text[match.numberEnd:match.matchEnd])
	switch {
	case containsAny(marker, "$", "\U0001F4B2", "usd", "дол"):
		return currencyUSD
	case containsAny(marker, "₾", "gel", "lari", "лари"):
		return currencyGEL
	default:
		return currencyNone
	}
}

func findPriceMatches(text string, pattern *regexp.Regexp, accept func(priceMatch) (priceMatch, bool)) []priceMatch {
	var matches []priceMatch
	for _, indexes := range pattern.FindAllStringSubmatchIndex(text, -1) {
		value, ok := parsePriceNumber(text[indexes[2]:indexes[3]])
		if !ok {
			continue
		}
		match := priceMatch{text: text, matchStart: indexes[0], matchEnd: indexes[1], numberStart: indexes[2], numberEnd: indexes[3], value: value}
		if accepted, ok := accept(match); ok {
			matches = append(matches, accepted)
		}
	}
	return matches
}

func currencyClaimedByNumberFirst(currencyFirst priceMatch, numberFirst []priceMatch) bool {
	for _, match := range numberFirst {
		if currencyFirst.matchStart >= match.matchStart && currencyFirst.matchStart < match.matchEnd {
			return true
		}
	}
	return false
}

func acceptNumberFirstPrice(match priceMatch) (priceMatch, bool) {
	prefix := match.text[:match.numberStart]
	if match.value > maxPriceValue || digitGroupBeforePattern.MatchString(prefix) {
		return match, false
	}
	if isGluedToIdentifier(prefix) {
		return dropLeadingGroup(match)
	}
	if leadingGroupBelongsToContext(prefix) {
		if !isGroupedNumber(match) {
			return match, true
		}
		return dropLeadingGroup(match)
	}
	return match, true
}

func isGroupedNumber(match priceMatch) bool {
	return strings.ContainsAny(match.text[match.numberStart:match.numberEnd], " .,")
}

func acceptCurrencyFirstPrice(match priceMatch) (priceMatch, bool) {
	match, ok := dropTrailingGroupBeforeUnit(match)
	if !ok {
		return match, false
	}
	return match, !structureAfterPricePattern.MatchString(match.text[match.numberEnd:])
}

func acceptKeywordOnlyPrice(match priceMatch) (priceMatch, bool) {
	match, ok := dropTrailingGroupBeforeUnit(match)
	if !ok || match.value < minKeywordOnlyPrice || match.value > maxPriceValue {
		return match, false
	}
	rest := match.text[match.numberEnd:]
	if unitAfterPricePattern.MatchString(rest) || percentAfterPricePattern.MatchString(rest) || yearAfterPricePattern.MatchString(rest) || phoneTailAfterPricePattern.MatchString(rest) {
		return match, false
	}
	return match, !startsWithArea(match.text[match.numberStart:])
}

func leadingGroupBelongsToContext(prefix string) bool {
	return unitBeforeNumberPattern.MatchString(prefix) || floorBeforeNumberPattern.MatchString(prefix)
}

func dropLeadingGroup(match priceMatch) (priceMatch, bool) {
	firstGroupEnd := strings.IndexAny(match.text[match.numberStart:match.numberEnd], " .,")
	if firstGroupEnd == -1 {
		return match, false
	}
	match.numberStart += firstGroupEnd + 1
	return reparsePriceMatch(match)
}

func dropTrailingGroupBeforeUnit(match priceMatch) (priceMatch, bool) {
	if !unitAfterPricePattern.MatchString(match.text[match.numberEnd:]) {
		return match, true
	}
	lastGroupStart := strings.LastIndexAny(match.text[match.numberStart:match.numberEnd], " .,")
	if lastGroupStart == -1 {
		return match, true
	}
	match.numberEnd = match.numberStart + lastGroupStart
	return reparsePriceMatch(match)
}

func reparsePriceMatch(match priceMatch) (priceMatch, bool) {
	value, ok := parsePriceNumber(match.text[match.numberStart:match.numberEnd])
	match.value = value
	return match, ok
}

func isGluedToIdentifier(prefix string) bool {
	tokenStart := strings.LastIndexAny(prefix, " \t\n") + 1
	token := prefix[tokenStart:]
	if token == "" {
		return false
	}
	return strings.ContainsAny(token, "+/№") || strings.Count(token, "-") >= 2
}

func parsePriceNumber(raw string) (int, bool) {
	digits := strings.NewReplacer(" ", "", ",", "", ".", "").Replace(raw)
	value, err := strconv.Atoi(digits)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func startsWithArea(text string) bool {
	location := areaPattern.FindStringIndex(text)
	return location != nil && location[0] == 0
}

func isDepositContext(text string, numberStart int) bool {
	lineStart := strings.LastIndex(text[:numberStart], "\n") + 1
	linePrefix := strings.ToLower(text[lineStart:numberStart])
	for _, depositKeyword := range []string{"депозит", "залог", "deposit", "комиссия", "commission", "предоплат", "prepay", "страховк"} {
		idx := strings.LastIndex(linePrefix, depositKeyword)
		if idx == -1 {
			continue
		}
		if !containsAny(linePrefix[idx+len(depositKeyword):], "цена", "price", "стоимость", "rent") {
			return true
		}
	}
	return false
}

func parseArea(text string) float64 {
	match := areaPattern.FindStringSubmatch(text)
	if len(match) < 2 {
		return 0
	}
	value := strings.ReplaceAll(match[1], ",", ".")
	area, _ := strconv.ParseFloat(value, 64)
	return area
}
