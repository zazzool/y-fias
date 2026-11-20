package store

// Хранение флагов переключения чтения (этап 4).

import (
	"context"
	"fmt"

	"github.com/zazzool/y-fias/internal/rollout"
)

// LoadRollout читает сохранённые доли.
func (s *Store) LoadRollout(ctx context.Context) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT endpoint, percent FROM rollout_flags`)
	if err != nil {
		return nil, fmt.Errorf("чтение флагов: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var (
			e string
			p int16
		)
		if err := rows.Scan(&e, &p); err != nil {
			return nil, fmt.Errorf("чтение флагов: %w", err)
		}
		out[e] = int(p)
	}
	return out, rows.Err()
}

// SaveRollout записывает новую долю и строку журнала одной транзакцией:
// доля без записи в журнале — это раскатка, которую потом нельзя объяснить.
func (s *Store) SaveRollout(ctx context.Context, c rollout.Change) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("сохранение флага: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		INSERT INTO rollout_flags (endpoint, percent, updated_at, updated_by)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (endpoint) DO UPDATE
		SET percent = EXCLUDED.percent,
		    updated_at = EXCLUDED.updated_at,
		    updated_by = EXCLUDED.updated_by`,
		c.Endpoint, c.To, c.At, c.Actor); err != nil {
		return fmt.Errorf("сохранение флага: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO rollout_changes (ts, endpoint, from_percent, to_percent, reason, actor)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, ''))`,
		c.At, c.Endpoint, c.From, c.To, c.Reason, c.Actor); err != nil {
		return fmt.Errorf("журнал флагов: %w", err)
	}
	return tx.Commit(ctx)
}

// RolloutHistory — последние изменения, новые первыми.
func (s *Store) RolloutHistory(ctx context.Context, limit int) ([]rollout.Change, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ts, endpoint, from_percent, to_percent,
		       COALESCE(reason, ''), COALESCE(actor, '')
		FROM rollout_changes
		ORDER BY ts DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("журнал флагов: %w", err)
	}
	defer rows.Close()

	var out []rollout.Change
	for rows.Next() {
		var (
			c        rollout.Change
			from, to int16
		)
		if err := rows.Scan(&c.At, &c.Endpoint, &from, &to, &c.Reason, &c.Actor); err != nil {
			return nil, fmt.Errorf("журнал флагов: %w", err)
		}
		c.From, c.To = int(from), int(to)
		out = append(out, c)
	}
	return out, rows.Err()
}
