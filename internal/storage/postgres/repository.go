package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"
	"apartment-hunter/internal/profiles"
	"apartment-hunter/internal/sources"

	_ "github.com/lib/pq"
)

type Repository struct {
	db *sql.DB
}

func Open(ctx context.Context, dsn string) (*Repository, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(3)
	db.SetConnMaxLifetime(time.Hour)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	repo := &Repository{db: db}
	if err := repo.Migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return repo, nil
}

func (r *Repository) Close() error {
	return r.db.Close()
}

func (r *Repository) Migrate(ctx context.Context) error {
	_, err := r.db.ExecContext(ctx, schemaSQL)
	return err
}

func (r *Repository) UpsertProfile(ctx context.Context, profile profiles.Profile) error {
	var preferencesJSON any
	if profile.Preferences != nil {
		data, err := json.Marshal(profile.Preferences)
		if err != nil {
			return err
		}
		preferencesJSON = data
	}
	var debugMode any
	if profile.DebugMode != nil {
		debugMode = *profile.DebugMode
	}
	var mcpMode any
	if profile.MCPMode != nil {
		mcpMode = *profile.MCPMode
	}
	var filterDisabled any
	if profile.FilterDisabled != nil {
		filterDisabled = *profile.FilterDisabled
	}
	var mcpHistoryJSON any
	if profile.MCPHistory != nil {
		data, err := json.Marshal(profile.MCPHistory)
		if err != nil {
			return err
		}
		if string(data) == "null" {
			data = []byte("[]")
		}
		mcpHistoryJSON = data
	}
	_, err := r.db.ExecContext(ctx, `
		insert into profiles (id, telegram_chat_id, preferences, debug_mode, mcp_mode, filter_disabled, mcp_history)
		values ($1, $2, $3, coalesce($4, true), coalesce($5, false), coalesce($6, false), coalesce($7, '[]'::jsonb))
		on conflict (id) do update set
			telegram_chat_id = coalesce(excluded.telegram_chat_id, profiles.telegram_chat_id),
			preferences = coalesce(excluded.preferences, profiles.preferences),
			debug_mode = coalesce($4, profiles.debug_mode),
			mcp_mode = coalesce($5, profiles.mcp_mode),
			filter_disabled = coalesce($6, profiles.filter_disabled),
			mcp_history = coalesce($7, profiles.mcp_history),
			updated_at = now()
	`, profile.ID, nullInt64(profile.TelegramChatID), preferencesJSON, debugMode, mcpMode, filterDisabled, mcpHistoryJSON)
	return err
}

func (r *Repository) GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error) {
	var profile profiles.Profile
	var preferencesJSON []byte
	var debugMode bool
	var mcpMode bool
	var filterDisabled bool
	var mcpHistoryJSON []byte
	err := r.db.QueryRowContext(ctx, `
		select id, coalesce(telegram_chat_id, 0), preferences, debug_mode, mcp_mode, filter_disabled, mcp_history
		from profiles
		where id = $1
	`, id).Scan(&profile.ID, &profile.TelegramChatID, &preferencesJSON, &debugMode, &mcpMode, &filterDisabled, &mcpHistoryJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return profiles.Profile{}, false, nil
		}
		return profiles.Profile{}, false, err
	}
	profile.DebugMode = &debugMode
	profile.MCPMode = &mcpMode
	profile.FilterDisabled = &filterDisabled
	if len(preferencesJSON) > 0 {
		var prefs domain.UserPreferences
		if err := json.Unmarshal(preferencesJSON, &prefs); err != nil {
			return profiles.Profile{}, false, err
		}
		var hasArea bool
		err := r.db.QueryRowContext(ctx, `
			select search_area is not null
			from user_search_profiles
			where user_id = $1
			order by created_at desc, id desc
			limit 1
		`, id).Scan(&hasArea)
		if err == nil && hasArea {
			prefs.HasSearchArea = true
		}
		profile.Preferences = &prefs
	}
	if len(mcpHistoryJSON) > 0 {
		var history []profiles.MCPMessage
		if err := json.Unmarshal(mcpHistoryJSON, &history); err != nil {
			return profiles.Profile{}, false, err
		}
		profile.MCPHistory = history
	}
	channels, err := r.listProfileChannels(ctx, id)
	if err != nil {
		return profiles.Profile{}, false, err
	}
	profile.Channels = channels
	return profile, true, nil
}

func (r *Repository) SaveSearchProfile(ctx context.Context, profile domain.UserSearchProfile) error {
	preferencesJSON, err := json.Marshal(profile.Preferences)
	if err != nil {
		return err
	}

	var latestPreferencesJSON []byte
	var latestSearchAreaWKT sql.NullString
	latestErr := r.db.QueryRowContext(ctx, `
		select preferences_json, ST_AsText(search_area)
		from user_search_profiles
		where user_id = $1
		order by created_at desc, id desc
		limit 1
	`, profile.UserID).Scan(&latestPreferencesJSON, &latestSearchAreaWKT)

	var searchAreaWKT string
	if profile.SearchArea != nil && strings.TrimSpace(profile.SearchArea.WKT) != "" {
		searchAreaWKT = profile.SearchArea.WKT
	} else if latestErr == nil && latestSearchAreaWKT.Valid {
		searchAreaWKT = latestSearchAreaWKT.String
	}

	if latestErr == nil {
		var latestPrefs domain.UserPreferences
		if unmarshalErr := json.Unmarshal(latestPreferencesJSON, &latestPrefs); unmarshalErr == nil {
			wktMatches := false
			if !latestSearchAreaWKT.Valid && searchAreaWKT == "" {
				wktMatches = true
			} else if latestSearchAreaWKT.Valid && searchAreaWKT != "" {
				wktMatches = latestSearchAreaWKT.String == searchAreaWKT
			}

			if wktMatches && reflect.DeepEqual(latestPrefs, profile.Preferences) {
				// Duplicate detected. Skip inserting.
				return nil
			}
		}
	}

	var searchAreaWKTVal any
	if searchAreaWKT != "" {
		searchAreaWKTVal = searchAreaWKT
	} else {
		searchAreaWKTVal = nil
	}

	_, err = r.db.ExecContext(ctx, `
		insert into user_search_profiles (user_id, raw_query, normalized_query, preferences_json, search_area)
		values ($1, $2, $3, $4, case when $5::text is null then null else ST_GeomFromText($5, 4326) end)
	`, profile.UserID, profile.RawQuery, profile.NormalizedQuery, preferencesJSON, searchAreaWKTVal)
	return err
}

func (r *Repository) LatestSearchAreaGeoJSON(ctx context.Context, profileID string) (string, bool, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return "", false, nil
	}
	var geoJSON sql.NullString
	err := r.db.QueryRowContext(ctx, `
		select ST_AsGeoJSON(search_area)
		from user_search_profiles
		where user_id = $1
		order by created_at desc, id desc
		limit 1
	`, profileID).Scan(&geoJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", false, nil
		}
		return "", false, err
	}
	return geoJSON.String, geoJSON.Valid && strings.TrimSpace(geoJSON.String) != "", nil
}

func (r *Repository) MatchesListingLocation(ctx context.Context, profileID string, listing domain.ApartmentListing) (bool, string, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return true, "", nil
	}

	var hasArea bool
	err := r.db.QueryRowContext(ctx, `
		select search_area is not null
		from user_search_profiles
		where user_id = $1
		order by created_at desc, id desc
		limit 1
	`, profileID).Scan(&hasArea)
	if err != nil {
		if err == sql.ErrNoRows {
			return true, "", nil
		}
		return false, "", err
	}
	if !hasArea {
		return true, "", nil
	}
	if listing.Latitude == 0 && listing.Longitude == 0 {
		return false, "listing location is unresolved", nil
	}

	var matches bool
	err = r.db.QueryRowContext(ctx, `
		with latest as (
			select search_area
			from user_search_profiles
			where user_id = $1
			order by created_at desc, id desc
			limit 1
		)
		select ST_Covers(
			search_area,
			ST_SetSRID(ST_MakePoint($2, $3), 4326)
		)
		from latest
	`, profileID, listing.Longitude, listing.Latitude).Scan(&matches)
	if err != nil {
		return false, "", err
	}
	if !matches {
		return false, "listing location is outside preferred search area", nil
	}
	return true, "", nil
}

func (r *Repository) UpsertProfileChannel(ctx context.Context, profileID string, sub profiles.ChannelSubscription) (bool, error) {
	sub = normalizeSubscription(sub)
	exists, err := r.profileChannelExists(ctx, profileID, sub.Source)
	if err != nil {
		return false, err
	}
	if err := r.UpsertProfile(ctx, profiles.Profile{ID: profileID}); err != nil {
		return false, err
	}
	cursorJSON, err := json.Marshal(sub.Cursor)
	if err != nil {
		return false, err
	}
	_, err = r.db.ExecContext(ctx, `
		insert into profile_channels (profile_id, channel_id, username, title, cursor, limit_count)
		values ($1, $2, $3, $4, $5, $6)
		on conflict (profile_id, channel_id) do update set
			username = excluded.username,
			title = excluded.title,
			limit_count = excluded.limit_count,
			updated_at = now()
	`, profileID, sub.Source.ID, nullString(sub.Source.Username), nullString(sub.Source.Title), cursorJSON, sub.Limit)
	return !exists, err
}

func (r *Repository) RemoveProfileChannel(ctx context.Context, profileID, channel string) (bool, error) {
	channel = strings.TrimPrefix(strings.TrimSpace(channel), "@")
	result, err := r.db.ExecContext(ctx, `
		delete from profile_channels
		where profile_id = $1 and (lower(channel_id) = lower($2) or lower(coalesce(username, '')) = lower($2))
	`, profileID, channel)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func (r *Repository) UpdateProfileChannelCursor(ctx context.Context, profileID string, source sources.ChannelRef, cursor sources.Cursor) error {
	source = normalizeChannel(source)
	cursorJSON, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(ctx, `
		update profile_channels
		set cursor = $3, updated_at = now()
		where profile_id = $1 and channel_id = $2
	`, profileID, source.ID, cursorJSON)
	return err
}

func (r *Repository) UpdateProfileChannelSource(ctx context.Context, profileID string, current, resolved sources.ChannelRef) error {
	current = normalizeChannel(current)
	resolved = normalizeChannel(resolved)
	if strings.TrimSpace(profileID) == "" || current.ID == "" || resolved.ID == "" || current.ID == resolved.ID {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		update profile_channels
		set channel_id = $3,
			username = coalesce($4, username),
			title = coalesce($5, title),
			updated_at = now()
		where profile_id = $1 and channel_id = $2
	`, profileID, current.ID, resolved.ID, nullString(resolved.Username), nullString(resolved.Title))
	return err
}

func (r *Repository) listProfileChannels(ctx context.Context, profileID string) ([]profiles.ChannelSubscription, error) {
	rows, err := r.db.QueryContext(ctx, `
		select channel_id, coalesce(username, ''), coalesce(title, ''), cursor, limit_count
		from profile_channels
		where profile_id = $1
		order by created_at asc
	`, profileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var channels []profiles.ChannelSubscription
	for rows.Next() {
		var sub profiles.ChannelSubscription
		var cursorJSON []byte
		if err := rows.Scan(&sub.Source.ID, &sub.Source.Username, &sub.Source.Title, &cursorJSON, &sub.Limit); err != nil {
			return nil, err
		}
		if len(cursorJSON) > 0 {
			if err := json.Unmarshal(cursorJSON, &sub.Cursor); err != nil {
				return nil, err
			}
		}
		channels = append(channels, sub)
	}
	return channels, rows.Err()
}

func (r *Repository) profileChannelExists(ctx context.Context, profileID string, source sources.ChannelRef) (bool, error) {
	source = normalizeChannel(source)
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		select exists (
			select 1 from profile_channels
			where profile_id = $1 and channel_id = $2
		)
	`, profileID, source.ID).Scan(&exists)
	return exists, err
}

func (r *Repository) SaveEvaluation(ctx context.Context, record domain.ListingEvaluationRecord) error {
	listingJSON, err := json.Marshal(record.Listing)
	if err != nil {
		return err
	}
	sourceJSON, err := json.Marshal(record.Source)
	if err != nil {
		return err
	}
	scoreJSON, err := marshalNullable(record.Score)
	if err != nil {
		return err
	}
	insightsJSON, err := marshalNullable(record.Insights)
	if err != nil {
		return err
	}
	usageJSON, err := marshalNullable(record.AIUsage)
	if err != nil {
		return err
	}
	duplicateJSON, err := marshalNullable(record.Duplicate)
	if err != nil {
		return err
	}

	_, err = r.db.ExecContext(ctx, `
		insert into listing_evaluations (
			listing_id, profile_id, telegram_chat_id, source, channel_id, channel_name,
			message_id, message_link, listing, score, insights, ai_usage, duplicate,
			report, status, failure_reason, ai_provider, ai_model, input_tokens,
			cached_input_tokens, output_tokens, reasoning_tokens, total_tokens,
			input_price_per_million_usd, cached_input_price_per_million_usd,
			output_price_per_million_usd, cost_usd
		) values (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, $19,
			$20, $21, $22, $23,
			$24, $25, $26, $27
		)
		on conflict (listing_id) do update set
			profile_id = excluded.profile_id,
			telegram_chat_id = excluded.telegram_chat_id,
			source = excluded.source,
			channel_id = excluded.channel_id,
			channel_name = excluded.channel_name,
			message_id = excluded.message_id,
			message_link = excluded.message_link,
			listing = excluded.listing,
			score = excluded.score,
			insights = excluded.insights,
			ai_usage = excluded.ai_usage,
			duplicate = excluded.duplicate,
			report = excluded.report,
			status = excluded.status,
			failure_reason = excluded.failure_reason,
			ai_provider = excluded.ai_provider,
			ai_model = excluded.ai_model,
			input_tokens = excluded.input_tokens,
			cached_input_tokens = excluded.cached_input_tokens,
			output_tokens = excluded.output_tokens,
			reasoning_tokens = excluded.reasoning_tokens,
			total_tokens = excluded.total_tokens,
			input_price_per_million_usd = excluded.input_price_per_million_usd,
			cached_input_price_per_million_usd = excluded.cached_input_price_per_million_usd,
			output_price_per_million_usd = excluded.output_price_per_million_usd,
			cost_usd = excluded.cost_usd,
			updated_at = now()
	`, record.Listing.ID, record.Source.ProfileID, nullInt64(record.Source.TelegramChatID), sourceJSON,
		record.Source.ChannelID, record.Source.ChannelName, nullInt(record.Source.MessageID),
		record.Source.MessageLink, listingJSON, scoreJSON, insightsJSON, usageJSON, duplicateJSON,
		record.Report, record.Status, record.FailureReason, usageField(record.AIUsage, func(u domain.AIUsage) string { return u.Provider }),
		usageField(record.AIUsage, func(u domain.AIUsage) string { return u.Model }),
		usageInt(record.AIUsage, func(u domain.AIUsage) int { return u.InputTokens }),
		usageInt(record.AIUsage, func(u domain.AIUsage) int { return u.CachedInputTokens }),
		usageInt(record.AIUsage, func(u domain.AIUsage) int { return u.OutputTokens }),
		usageInt(record.AIUsage, func(u domain.AIUsage) int { return u.ReasoningTokens }),
		usageInt(record.AIUsage, func(u domain.AIUsage) int { return u.TotalTokens }),
		usageFloat(record.AIUsage, func(u domain.AIUsage) float64 { return u.InputPricePerMillionUSD }),
		usageFloat(record.AIUsage, func(u domain.AIUsage) float64 { return u.CachedInputPricePerMillionUSD }),
		usageFloat(record.AIUsage, func(u domain.AIUsage) float64 { return u.OutputPricePerMillionUSD }),
		usageFloat(record.AIUsage, func(u domain.AIUsage) float64 { return u.CostUSD }))
	return err
}

func (r *Repository) SaveListingFeedback(ctx context.Context, feedback domain.ListingFeedback) (string, *domain.ApartmentListing, error) {
	feedback.Text = strings.TrimSpace(feedback.Text)
	if feedback.Text == "" {
		return "", nil, fmt.Errorf("feedback text is required")
	}
	if feedback.TelegramChatID == 0 || feedback.FeedbackMessageID == 0 || feedback.RepliedMessageID == 0 {
		return "", nil, fmt.Errorf("telegram chat id, feedback message id and replied message id are required")
	}

	var evaluationID, listingID, profileID string
	var listingJSON string
	err := r.db.QueryRowContext(ctx, `
		select id::text, listing_id, coalesce(profile_id, ''), listing::text
		from listing_evaluations
		where telegram_chat_id = $1
			and (bot_message_id = $2 or message_id = $2)
		order by updated_at desc, id desc
		limit 1
	`, feedback.TelegramChatID, feedback.RepliedMessageID).Scan(&evaluationID, &listingID, &profileID, &listingJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", nil, nil
		}
		return "", nil, err
	}

	var listing domain.ApartmentListing
	if err := json.Unmarshal([]byte(listingJSON), &listing); err != nil {
		return "", nil, fmt.Errorf("failed to parse listing json: %w", err)
	}

	var id string
	err = r.db.QueryRowContext(ctx, `
		insert into listing_feedbacks (
			evaluation_id, listing_id, profile_id, telegram_chat_id,
			feedback_message_id, replied_message_id, feedback_text
		) values ($1::bigint, $2, $3, $4, $5, $6, $7)
		on conflict (telegram_chat_id, feedback_message_id) do update set
			feedback_text = excluded.feedback_text,
			updated_at = now()
		returning id::text
	`, evaluationID, listingID, nullString(profileID), feedback.TelegramChatID,
		feedback.FeedbackMessageID, feedback.RepliedMessageID, feedback.Text).Scan(&id)
	return id, &listing, err
}

func (r *Repository) UpdateEvaluationNotificationMessageID(ctx context.Context, listingID string, botMessageID int) error {
	if strings.TrimSpace(listingID) == "" || botMessageID == 0 {
		return nil
	}
	_, err := r.db.ExecContext(ctx, `
		update listing_evaluations
		set bot_message_id = $2, updated_at = now()
		where listing_id = $1
	`, listingID, botMessageID)
	return err
}

func (r *Repository) ListOpenFeedbacks(ctx context.Context) ([]domain.ListingFeedback, error) {
	rows, err := r.db.QueryContext(ctx, `
		select id::text, coalesce(evaluation_id::text, ''), coalesce(listing_id, ''),
			coalesce(profile_id, ''), telegram_chat_id, feedback_message_id,
			replied_message_id, feedback_text, status, coalesce(resolved_commit, ''),
			coalesce(resolution_note, '')
		from listing_feedbacks
		where status = 'open'
		order by created_at asc, id asc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []domain.ListingFeedback
	for rows.Next() {
		var item domain.ListingFeedback
		if err := rows.Scan(&item.ID, &item.EvaluationID, &item.ListingID, &item.ProfileID,
			&item.TelegramChatID, &item.FeedbackMessageID, &item.RepliedMessageID,
			&item.Text, &item.Status, &item.ResolvedCommit, &item.ResolutionNote); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *Repository) ResolveFeedback(ctx context.Context, id, commit, note string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("feedback id is required")
	}
	_, err := r.db.ExecContext(ctx, `
		update listing_feedbacks
		set status = 'resolved',
			resolved_commit = $2,
			resolution_note = $3,
			resolved_at = now(),
			updated_at = now()
		where id = $1 and status = 'open'
	`, id, nullString(commit), nullString(note))
	return err
}

func (r *Repository) EvaluationExists(ctx context.Context, profileID, channelID string, messageID int) (bool, error) {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(channelID) == "" || messageID == 0 {
		return false, nil
	}
	var exists bool
	err := r.db.QueryRowContext(ctx, `
		select exists (
			select 1 from listing_evaluations
			where profile_id = $1 and channel_id = $2 and message_id = $3
		)
	`, profileID, channelID, messageID).Scan(&exists)
	return exists, err
}

func (r *Repository) SaveImageFingerprints(ctx context.Context, listingID string, fingerprints []dedup.ListingImageFingerprint) error {
	for _, fingerprint := range fingerprints {
		hashesJSON, err := json.Marshal(fingerprint.Hashes)
		if err != nil {
			return err
		}
		_, err = r.db.ExecContext(ctx, `
			insert into listing_images (
				listing_id, source_image_url, hashes, sha256, phash_full,
				phash_center_80, phash_center_60, dhash_full
			) values ($1, $2, $3, $4, $5, $6, $7, $8)
			on conflict (listing_id, source_image_url) do update set
				hashes = excluded.hashes,
				sha256 = excluded.sha256,
				phash_full = excluded.phash_full,
				phash_center_80 = excluded.phash_center_80,
				phash_center_60 = excluded.phash_center_60,
				dhash_full = excluded.dhash_full
		`, listingID, fingerprint.ImageURL, hashesJSON, fingerprint.Hashes.SHA256,
			fingerprint.Hashes.PHashFull, fingerprint.Hashes.PHashCenter80,
			fingerprint.Hashes.PHashCenter60, fingerprint.Hashes.DHashFull)
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) ListImageFingerprints(ctx context.Context) ([]dedup.StoredListingImage, error) {
	rows, err := r.db.QueryContext(ctx, `
		select id::text, listing_id, source_image_url, hashes, created_at
		from listing_images
		order by created_at asc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []dedup.StoredListingImage
	for rows.Next() {
		var item dedup.StoredListingImage
		var hashesJSON []byte
		if err := rows.Scan(&item.ID, &item.ListingID, &item.SourceImageURL, &hashesJSON, &item.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(hashesJSON, &item.Hashes); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (r *Repository) SaveDuplicate(ctx context.Context, listingID string, candidate dedup.DuplicateCandidate) error {
	_, err := r.db.ExecContext(ctx, `
		insert into listing_duplicates (
			listing_id, canonical_listing_id, matched_images, strong_matches,
			weak_matches, confidence, reason
		) values ($1, $2, $3, $4, $5, $6, $7)
		on conflict (listing_id) do update set
			canonical_listing_id = excluded.canonical_listing_id,
			matched_images = excluded.matched_images,
			strong_matches = excluded.strong_matches,
			weak_matches = excluded.weak_matches,
			confidence = excluded.confidence,
			reason = excluded.reason
	`, listingID, candidate.CanonicalListingID, candidate.MatchedImages, candidate.StrongMatches,
		candidate.WeakMatches, candidate.Confidence, candidate.Reason)
	return err
}

func (r *Repository) ListDuplicates(ctx context.Context) ([]dedup.ListingDuplicateRecord, error) {
	rows, err := r.db.QueryContext(ctx, `
		select id::text, listing_id, canonical_listing_id, confidence, reason, created_at
		from listing_duplicates
		order by created_at asc
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []dedup.ListingDuplicateRecord
	for rows.Next() {
		var item dedup.ListingDuplicateRecord
		if err := rows.Scan(&item.ID, &item.ListingID, &item.CanonicalListingID, &item.Confidence, &item.Reason, &item.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func marshalNullable(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func nullInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullInt64(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullString(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func normalizeSubscription(sub profiles.ChannelSubscription) profiles.ChannelSubscription {
	if sub.Limit <= 0 {
		sub.Limit = 50
	}
	sub.Source = normalizeChannel(sub.Source)
	return sub
}

func normalizeChannel(source sources.ChannelRef) sources.ChannelRef {
	source.Username = strings.TrimPrefix(strings.TrimSpace(source.Username), "@")
	source.ID = strings.TrimPrefix(strings.TrimSpace(source.ID), "@")
	if source.ID == "" {
		source.ID = source.Username
	}
	if source.Username == "" {
		source.Username = source.ID
	}
	return source
}

func usageField(usage *domain.AIUsage, getter func(domain.AIUsage) string) any {
	if usage == nil {
		return nil
	}
	return getter(*usage)
}

func usageInt(usage *domain.AIUsage, getter func(domain.AIUsage) int) any {
	if usage == nil {
		return 0
	}
	return getter(*usage)
}

func usageFloat(usage *domain.AIUsage, getter func(domain.AIUsage) float64) any {
	if usage == nil {
		return 0
	}
	return getter(*usage)
}

func (r *Repository) SearchEvaluations(ctx context.Context, profileID string, filter domain.EvaluationSearchFilter) ([]domain.ListingEvaluationRecord, error) {
	queryParts := []string{`
		select listing, source, score, insights, ai_usage, duplicate, coalesce(report, ''), status, coalesce(failure_reason, '')
		from listing_evaluations
		where profile_id = $1
	`}
	args := []any{profileID}

	if filter.Status != "" {
		args = append(args, filter.Status)
		queryParts = append(queryParts, fmt.Sprintf("and status = $%d", len(args)))
	}

	if filter.MinPrice > 0 {
		args = append(args, filter.MinPrice)
		queryParts = append(queryParts, fmt.Sprintf("and (listing->>'price')::integer >= $%d", len(args)))
	}

	if filter.MaxPrice > 0 {
		args = append(args, filter.MaxPrice)
		queryParts = append(queryParts, fmt.Sprintf("and (listing->>'price')::integer <= $%d", len(args)))
	}

	if filter.Location != "" {
		args = append(args, "%"+strings.ToLower(filter.Location)+"%")
		queryParts = append(queryParts, fmt.Sprintf("and (lower(listing->>'district') ilike $%d or lower(listing->>'title') ilike $%d or lower(listing->>'description') ilike $%d)", len(args), len(args), len(args)))
	}

	if filter.Query != "" {
		args = append(args, "%"+strings.ToLower(filter.Query)+"%")
		queryParts = append(queryParts, fmt.Sprintf("and (lower(listing->>'title') ilike $%d or lower(listing->>'description') ilike $%d or lower(coalesce(report, '')) ilike $%d or lower(coalesce(failure_reason, '')) ilike $%d or lower(coalesce(insights::text, '')) ilike $%d)", len(args), len(args), len(args), len(args), len(args)))
	}

	queryParts = append(queryParts, "order by created_at desc")

	limit := 10
	if filter.Limit > 0 {
		limit = filter.Limit
	}
	args = append(args, limit)
	queryParts = append(queryParts, fmt.Sprintf("limit $%d", len(args)))

	query := strings.Join(queryParts, "\n")
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var records []domain.ListingEvaluationRecord
	for rows.Next() {
		var record domain.ListingEvaluationRecord
		var listingJSON []byte
		var sourceJSON []byte
		var scoreJSON []byte
		var insightsJSON []byte
		var aiUsageJSON []byte
		var duplicateJSON []byte

		err := rows.Scan(
			&listingJSON,
			&sourceJSON,
			&scoreJSON,
			&insightsJSON,
			&aiUsageJSON,
			&duplicateJSON,
			&record.Report,
			&record.Status,
			&record.FailureReason,
		)
		if err != nil {
			return nil, err
		}

		if err := json.Unmarshal(listingJSON, &record.Listing); err != nil {
			return nil, fmt.Errorf("failed to unmarshal listing: %w", err)
		}
		if err := json.Unmarshal(sourceJSON, &record.Source); err != nil {
			return nil, fmt.Errorf("failed to unmarshal source: %w", err)
		}
		if len(scoreJSON) > 0 {
			var score domain.ListingScore
			if err := json.Unmarshal(scoreJSON, &score); err == nil {
				record.Score = &score
			}
		}
		if len(insightsJSON) > 0 {
			var insights domain.ListingInsights
			if err := json.Unmarshal(insightsJSON, &insights); err == nil {
				record.Insights = &insights
			}
		}
		if len(aiUsageJSON) > 0 {
			var aiUsage domain.AIUsage
			if err := json.Unmarshal(aiUsageJSON, &aiUsage); err == nil {
				record.AIUsage = &aiUsage
			}
		}
		if len(duplicateJSON) > 0 {
			var duplicate any
			if err := json.Unmarshal(duplicateJSON, &duplicate); err == nil {
				record.Duplicate = duplicate
			}
		}

		records = append(records, record)
	}

	return records, rows.Err()
}

func FormatDSN(host, port, user, password, dbname string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)
}
