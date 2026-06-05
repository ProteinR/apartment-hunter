package domain

type UserPreferences struct {
	MinBudget         int                  `json:"min_budget"`
	MaxBudget         int                  `json:"max_budget"`
	MinAreaM2         float64              `json:"min_area_m2"`
	RequireDishwasher bool                 `json:"require_dishwasher"`
	RequireOven       bool                 `json:"require_oven"`
	Locations         []LocationPreference `json:"locations"`
	DesiredStyle      string               `json:"desired_style"`
	Reject            RejectionPreferences `json:"reject"`
	Importance        ImportanceWeights    `json:"importance"`
}

type LocationPreference struct {
	Name              string  `json:"name"`
	MaxDistanceMeters int     `json:"max_distance_meters"`
	Latitude          float64 `json:"latitude,omitempty"`
	Longitude         float64 `json:"longitude,omitempty"`
}

type RejectionPreferences struct {
	GlossyCeilings     bool `json:"glossy_ceilings"`
	DarkWalls          bool `json:"dark_walls"`
	OldRenovation      bool `json:"old_renovation"`
	WindowsFacingHouse bool `json:"windows_facing_house"`
}

type ImportanceWeights struct {
	Location   int `json:"location"`
	Interior   int `json:"interior"`
	View       int `json:"view"`
	Price      int `json:"price"`
	Appliances int `json:"appliances"`
}

type ApartmentListing struct {
	ID            string           `json:"id"`
	Title         string           `json:"title"`
	Description   string           `json:"description"`
	Price         int              `json:"price"`
	AreaM2        float64          `json:"area_m2"`
	Latitude      float64          `json:"latitude"`
	Longitude     float64          `json:"longitude"`
	District      string           `json:"district"`
	HasDishwasher bool             `json:"has_dishwasher"`
	HasOven       bool             `json:"has_oven"`
	Photos        []ApartmentPhoto `json:"photos"`
}

type ListingSourceRef struct {
	Source         string `json:"source,omitempty"`
	ChannelID      string `json:"channel_id,omitempty"`
	ChannelName    string `json:"channel_name,omitempty"`
	MessageID      int    `json:"message_id,omitempty"`
	MessageLink    string `json:"message_link,omitempty"`
	GroupedID      int64  `json:"grouped_id,omitempty"`
	ProfileID      string `json:"profile_id,omitempty"`
	TelegramChatID int64  `json:"telegram_chat_id,omitempty"`
}

type ApartmentPhoto struct {
	URL     string `json:"url"`
	Room    string `json:"room"`
	Caption string `json:"caption"`
}

type ListingScore struct {
	PassedHardFilters bool               `json:"passed_hard_filters"`
	RejectionReasons  []string           `json:"rejection_reasons"`
	ScoreTotal        float64            `json:"score_total"`
	ScoreBreakdown    map[string]float64 `json:"score_breakdown"`
	ShortSummary      string             `json:"short_summary"`
}

type ListingInsights struct {
	Style                 string   `json:"style"`
	HasGlossyCeilings     bool     `json:"has_glossy_ceilings"`
	HasDarkWalls          bool     `json:"has_dark_walls"`
	HasOldRenovation      bool     `json:"has_old_renovation"`
	WindowsFacingHouse    bool     `json:"windows_facing_house"`
	ViewQuality           float64  `json:"view_quality"`
	InteriorQuality       float64  `json:"interior_quality"`
	DescriptionHighlights []string `json:"description_highlights"`
}

type AIUsage struct {
	Provider                      string  `json:"provider"`
	Model                         string  `json:"model"`
	InputTokens                   int     `json:"input_tokens"`
	CachedInputTokens             int     `json:"cached_input_tokens"`
	OutputTokens                  int     `json:"output_tokens"`
	ReasoningTokens               int     `json:"reasoning_tokens"`
	TotalTokens                   int     `json:"total_tokens"`
	InputPricePerMillionUSD       float64 `json:"input_price_per_million_usd"`
	CachedInputPricePerMillionUSD float64 `json:"cached_input_price_per_million_usd"`
	OutputPricePerMillionUSD      float64 `json:"output_price_per_million_usd"`
	CostUSD                       float64 `json:"cost_usd"`
}

type ListingEvaluationRecord struct {
	Listing       ApartmentListing `json:"listing"`
	Source        ListingSourceRef `json:"source"`
	Score         *ListingScore    `json:"score,omitempty"`
	Insights      *ListingInsights `json:"insights,omitempty"`
	AIUsage       *AIUsage         `json:"ai_usage,omitempty"`
	Duplicate     any              `json:"duplicate,omitempty"`
	Report        string           `json:"report,omitempty"`
	Status        string           `json:"status"`
	FailureReason string           `json:"failure_reason,omitempty"`
}
