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

	BudgetMin               *int                 `json:"budget_min,omitempty"`
	BudgetMax               *int                 `json:"budget_max,omitempty"`
	AreaMin                 *float64             `json:"area_min,omitempty"`
	AreaMax                 *float64             `json:"area_max,omitempty"`
	RequiredAmenities       []string             `json:"required_amenities"`
	PreferredAmenities      []string             `json:"preferred_amenities"`
	InteriorPreferences     InteriorPreferences  `json:"interior_preferences"`
	RejectRules             []string             `json:"reject_rules"`
	LocationPreferences     []LocationPreference `json:"location_preferences"`
	Weights                 PreferenceWeights    `json:"weights"`
	UnresolvedLocationRules []string             `json:"unresolved_location_rules"`
}

type LocationPreference struct {
	Name              string  `json:"name"`
	MaxDistanceMeters int     `json:"max_distance_meters"`
	Latitude          float64 `json:"latitude,omitempty"`
	Longitude         float64 `json:"longitude,omitempty"`

	Type               string   `json:"type,omitempty"`
	CenterName         string   `json:"center_name,omitempty"`
	RadiusMeters       *int     `json:"radius_meters,omitempty"`
	BoundaryStreetName *string  `json:"boundary_street_name,omitempty"`
	BoundaryRule       *string  `json:"boundary_rule,omitempty"`
	GeometryWKT        *string  `json:"geometry_wkt,omitempty"`
	GooglePlaceID      *string  `json:"google_place_id,omitempty"`
	Confidence         float64  `json:"confidence,omitempty"`
	RawText            string   `json:"raw_text,omitempty"`
	UnresolvedRules    []string `json:"unresolved_rules,omitempty"`
}

type InteriorPreferences struct {
	Style  []string `json:"style"`
	Reject []string `json:"reject"`
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

type PreferenceWeights struct {
	Location  int `json:"location"`
	Interior  int `json:"interior"`
	View      int `json:"view"`
	Price     int `json:"price"`
	Amenities int `json:"amenities"`
}

type ResolvedPlace struct {
	Name          string
	Latitude      float64
	Longitude     float64
	GooglePlaceID string
	Confidence    float64
}

type ResolvedStreet struct {
	Name          string
	Latitude      float64
	Longitude     float64
	GeometryWKT   string
	GooglePlaceID string
	Confidence    float64
}

type GeoSearchArea struct {
	WKT             string   `json:"wkt"`
	GeoJSON         string   `json:"geojson"`
	SRID            int      `json:"srid"`
	UnresolvedRules []string `json:"unresolved_rules"`
}

type UserSearchProfile struct {
	ID              string          `json:"id,omitempty"`
	UserID          string          `json:"user_id"`
	RawQuery        string          `json:"raw_query"`
	NormalizedQuery string          `json:"normalized_query"`
	Preferences     UserPreferences `json:"preferences"`
	SearchArea      *GeoSearchArea  `json:"search_area,omitempty"`
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
	Source          string `json:"source,omitempty"`
	ChannelID       string `json:"channel_id,omitempty"`
	ChannelName     string `json:"channel_name,omitempty"`
	ChannelUsername string `json:"channel_username,omitempty"`
	MessageID       int    `json:"message_id,omitempty"`
	MessageLink     string `json:"message_link,omitempty"`
	GroupedID       int64  `json:"grouped_id,omitempty"`
	ProfileID       string `json:"profile_id,omitempty"`
	TelegramChatID  int64  `json:"telegram_chat_id,omitempty"`
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
	HasDishwasher         bool     `json:"has_dishwasher"`
	HasOven               bool     `json:"has_oven"`
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

type ListingFeedback struct {
	ID                string `json:"id,omitempty"`
	EvaluationID      string `json:"evaluation_id,omitempty"`
	ListingID         string `json:"listing_id,omitempty"`
	ProfileID         string `json:"profile_id,omitempty"`
	TelegramChatID    int64  `json:"telegram_chat_id,omitempty"`
	FeedbackMessageID int    `json:"feedback_message_id,omitempty"`
	RepliedMessageID  int    `json:"replied_message_id,omitempty"`
	Text              string `json:"text"`
	Status            string `json:"status,omitempty"`
	ResolvedCommit    string `json:"resolved_commit,omitempty"`
	ResolutionNote    string `json:"resolution_note,omitempty"`
}
