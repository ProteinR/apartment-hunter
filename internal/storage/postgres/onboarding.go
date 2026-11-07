package postgres

import (
	"context"
	"time"

	"apartment-hunter/internal/profiles"
)

func (r *Repository) PopularSources(ctx context.Context, profileID, city string, limit int) ([]profiles.PopularSource, error) {
	rows, err := r.db.QueryContext(ctx, `
		with latest as (
			select distinct on (user_id) user_id, coalesce(nullif(preferences_json->>'city', ''), 'batumi') as city
			from user_search_profiles
			order by user_id, created_at desc
		), own as (
			select lower(coalesce(username, '')) as username
			from profile_channels
			where profile_id = $1
		)
		select lower(pc.username), max(coalesce(pc.title, '')), count(distinct pc.profile_id)
		from profile_channels pc
		join latest on latest.user_id = pc.profile_id
		where pc.profile_id <> $1
			and coalesce(pc.username, '') <> ''
			and latest.city = $2
			and pc.username !~ '^-?[0-9]+$'
			and lower(pc.username) not in (select username from own)
		group by lower(pc.username)
		having count(distinct pc.profile_id) >= 2
		order by count(distinct pc.profile_id) desc, lower(pc.username)
		limit $3
	`, profileID, city, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var popular []profiles.PopularSource
	for rows.Next() {
		var source profiles.PopularSource
		if err := rows.Scan(&source.Username, &source.Title, &source.Users); err != nil {
			return nil, err
		}
		popular = append(popular, source)
	}
	return popular, rows.Err()
}

func (r *Repository) QuietSearches(ctx context.Context, quietFor, maxQuiet time.Duration) ([]profiles.QuietSearch, error) {
	rows, err := r.db.QueryContext(ctx, `
		with latest_search as (
			select user_id, max(created_at) as saved_at
			from user_search_profiles
			group by user_id
		), last_notified as (
			select profile_id, max(created_at) as notified_at
			from listing_evaluations
			where bot_message_id is not null
			group by profile_id
		), last_source as (
			select profile_id, max(created_at) as added_at
			from profile_channels
			group by profile_id
		), quiet as (
			select p.id, p.telegram_chat_id, p.quiet_nudge_at,
				greatest(latest_search.saved_at, last_source.added_at, coalesce(last_notified.notified_at, latest_search.saved_at)) as quiet_since
			from profiles p
			join latest_search on latest_search.user_id = p.id
			join last_source on last_source.profile_id = p.id
			left join last_notified on last_notified.profile_id = p.id
			where p.telegram_chat_id is not null
				and p.debug_mode = false
				and p.filter_disabled = false
				and not exists (
					select 1 from bot_access ba
					where ba.telegram_chat_id = p.telegram_chat_id and ba.status <> 'allowed'
				)
		)
		select id, telegram_chat_id, quiet_since
		from quiet
		where quiet_since < now() - make_interval(secs => $1)
			and quiet_since > now() - make_interval(secs => $2)
			and (quiet_nudge_at is null or quiet_nudge_at < quiet_since)
	`, quietFor.Seconds(), maxQuiet.Seconds())
	if err != nil {
		return nil, err
	}
	var quiet []profiles.QuietSearch
	for rows.Next() {
		var search profiles.QuietSearch
		if err := rows.Scan(&search.ProfileID, &search.TelegramChatID, &search.QuietSince); err != nil {
			rows.Close()
			return nil, err
		}
		quiet = append(quiet, search)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range quiet {
		counts, err := r.evaluationStatusCountsSince(ctx, quiet[i].ProfileID, quiet[i].QuietSince)
		if err != nil {
			return nil, err
		}
		quiet[i].StatusCounts = counts
	}
	return quiet, nil
}

func (r *Repository) evaluationStatusCountsSince(ctx context.Context, profileID string, since time.Time) (map[string]int, error) {
	rows, err := r.db.QueryContext(ctx, `
		select status, count(*)
		from listing_evaluations
		where profile_id = $1 and created_at > $2
		group by status
	`, profileID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return nil, err
		}
		counts[status] = count
	}
	return counts, rows.Err()
}

func (r *Repository) MarkQuietNudgeSent(ctx context.Context, profileID string, quietSince time.Time) error {
	_, err := r.db.ExecContext(ctx, `update profiles set quiet_nudge_at = $2 where id = $1`, profileID, quietSince)
	return err
}
