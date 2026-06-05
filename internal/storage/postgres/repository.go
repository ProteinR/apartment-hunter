package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"apartment-hunter/internal/dedup"
	"apartment-hunter/internal/domain"

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
