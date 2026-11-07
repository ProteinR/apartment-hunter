package postgres

import (
	"context"
	"database/sql"
	"errors"

	"apartment-hunter/internal/access"
)

const accessColumns = `coalesce(telegram_chat_id, 0), username, display_name, status, updated_at`

func scanAccess(row interface{ Scan(...any) error }) (access.Entry, bool, error) {
	var entry access.Entry
	var status string
	if err := row.Scan(&entry.TelegramChatID, &entry.Username, &entry.DisplayName, &status, &entry.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return access.Entry{}, false, nil
		}
		return access.Entry{}, false, err
	}
	entry.Status = access.Status(status)
	return entry, true, nil
}

func (r *Repository) FindAccess(ctx context.Context, telegramChatID int64, username string) (access.Entry, bool, error) {
	username = access.NormalizeUsername(username)
	if telegramChatID == 0 {
		if username == "" {
			return access.Entry{}, false, nil
		}
		return scanAccess(r.db.QueryRowContext(ctx, `select `+accessColumns+` from bot_access where username = $1`, username))
	}
	return scanAccess(r.db.QueryRowContext(ctx, `
		select `+accessColumns+`
		from bot_access
		where telegram_chat_id = $1 or ($2 <> '' and telegram_chat_id is null and username = $2)
		order by (status = 'allowed') desc, (telegram_chat_id is not null) desc
		limit 1
	`, telegramChatID, username))
}

func (r *Repository) SaveAccess(ctx context.Context, entry access.Entry) (bool, error) {
	username := access.NormalizeUsername(entry.Username)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var id int64
	if entry.TelegramChatID != 0 {
		err = tx.QueryRowContext(ctx, `select id from bot_access where telegram_chat_id = $1 for update`, entry.TelegramChatID).Scan(&id)
	} else {
		err = tx.QueryRowContext(ctx, `select id from bot_access where username = $1 for update`, username).Scan(&id)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if entry.TelegramChatID != 0 && username != "" {
		if _, err := tx.ExecContext(ctx, `delete from bot_access where telegram_chat_id is null and username = $1`, username); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `update bot_access set username = '', updated_at = now() where username = $1 and id <> $2`, username, id); err != nil {
			return false, err
		}
	}

	created := false
	if id == 0 {
		var insertedID int64
		err = tx.QueryRowContext(ctx, `
			insert into bot_access (telegram_chat_id, username, display_name, status)
			values ($1, $2, $3, $4)
			on conflict (telegram_chat_id) do nothing
			returning id
		`, nullInt64(entry.TelegramChatID), username, entry.DisplayName, string(entry.Status)).Scan(&insertedID)
		if errors.Is(err, sql.ErrNoRows) {
			return false, tx.Commit()
		}
		created = err == nil
	} else {
		_, err = tx.ExecContext(ctx, `
			update bot_access set
				username = case when $2 <> '' then $2 else username end,
				display_name = case when $3 <> '' then $3 else display_name end,
				status = $4,
				updated_at = now()
			where id = $1
		`, id, username, entry.DisplayName, string(entry.Status))
	}
	if err != nil {
		return false, err
	}
	return created, tx.Commit()
}

func (r *Repository) ListAccess(ctx context.Context) ([]access.Entry, error) {
	rows, err := r.db.QueryContext(ctx, `select `+accessColumns+` from bot_access order by status, updated_at desc`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []access.Entry
	for rows.Next() {
		entry, _, err := scanAccess(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}
