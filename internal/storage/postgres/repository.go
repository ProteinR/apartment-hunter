package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
	_, err := r.db.ExecContext(ctx, `
		insert into profiles (id, telegram_chat_id, preferences)
		values ($1, $2, $3)
		on conflict (id) do update set
			telegram_chat_id = coalesce(excluded.telegram_chat_id, profiles.telegram_chat_id),
			preferences = coalesce(excluded.preferences, profiles.preferences),
			updated_at = now()
	`, profile.ID, nullInt64(profile.TelegramChatID), preferencesJSON)
	return err
}

func (r *Repository) GetProfile(ctx context.Context, id string) (profiles.Profile, bool, error) {
	var profile profiles.Profile
	var preferencesJSON []byte
	err := r.db.QueryRowContext(ctx, `
		select id, coalesce(telegram_chat_id, 0), preferences
		from profiles
		where id = $1
	`, id).Scan(&profile.ID, &profile.TelegramChatID, &preferencesJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return profiles.Profile{}, false, nil
		}
		return profiles.Profile{}, false, err
	}
	if len(preferencesJSON) > 0 {
		var prefs domain.UserPreferences
		if err := json.Unmarshal(preferencesJSON, &prefs); err != nil {
			return profiles.Profile{}, false, err
		}
		profile.Preferences = &prefs
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
	var searchAreaWKT any
	if profile.SearchArea != nil && strings.TrimSpace(profile.SearchArea.WKT) != "" {
		searchAreaWKT = profile.SearchArea.WKT
	}
	_, err = r.db.ExecContext(ctx, `
		insert into user_search_profiles (user_id, raw_query, normalized_query, preferences_json, search_area)
		values ($1, $2, $3, $4, case when $5::text is null then null else ST_GeomFromText($5, 4326) end)
	`, profile.UserID, profile.RawQuery, profile.NormalizedQuery, preferencesJSON, searchAreaWKT)
	return err
}

func (r *Repository) MatchesListingLocation(ctx context.Context, profileID string, listing domain.ApartmentListing) (bool, string, error) {
	profileID = strings.TrimSpace(profileID)
	if profileID == "" {
		return true, "", nil
	}

	var hasArea bool
	err := r.db.QueryRowContext(ctx, `
		select exists (
			select 1
			from user_search_profiles
			where user_id = $1 and search_area is not null
		)
	`, profileID).Scan(&hasArea)
	if err != nil {
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
			where user_id = $1 and search_area is not null
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
		return nil
	}
	return getter(*usage)
}

func usageFloat(usage *domain.AIUsage, getter func(domain.AIUsage) float64) any {
	if usage == nil {
		return nil
	}
	return getter(*usage)
}

func FormatDSN(host, port, user, password, dbname string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable", host, port, user, password, dbname)
}
