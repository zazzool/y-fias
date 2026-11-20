package store

import (
	"context"
	"fmt"
	"time"
)

// Журнал версий справочника и очередь расхождений.
//
// Состояние синхронизации хранится в базе, а не в файле рядом со снапшотом:
// файл мог бы разойтись с самим справочником, а по журналу видно, что и когда
// применялось и с каким результатом.

// GarVersion — запись о применённой публикации.
type GarVersion struct {
	VersionID int64
	AppliedAt time.Time
	Mode      string // delta | full
	Streets   int
	Houses    int
	MatchRate float64
	Note      string
}

// RecordVersion записывает применённую версию.
func (s *Store) RecordVersion(ctx context.Context, v GarVersion) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO gar_versions (version_id, mode, streets, houses, match_rate, note)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (version_id) DO UPDATE SET
			applied_at = now(),
			mode       = EXCLUDED.mode,
			streets    = EXCLUDED.streets,
			houses     = EXCLUDED.houses,
			match_rate = EXCLUDED.match_rate,
			note       = EXCLUDED.note`,
		v.VersionID, v.Mode, v.Streets, v.Houses, v.MatchRate, nullable(v.Note))
	if err != nil {
		return fmt.Errorf("запись версии %d: %w", v.VersionID, err)
	}
	return nil
}

// LastVersion — последняя применённая версия.
func (s *Store) LastVersion(ctx context.Context) (GarVersion, error) {
	var v GarVersion
	err := s.pool.QueryRow(ctx, `
		SELECT version_id, applied_at, mode,
		       COALESCE(streets, 0), COALESCE(houses, 0), COALESCE(match_rate, 0)
		  FROM gar_versions
		 ORDER BY version_id DESC
		 LIMIT 1`).Scan(&v.VersionID, &v.AppliedAt, &v.Mode, &v.Streets, &v.Houses, &v.MatchRate)
	return v, err
}

// PreviousMatchRate — доля наших адресов, найденных в ГАР, на предыдущей
// синхронизации.
//
// Нужна как сторож: резкое падение означает, что дельта применилась неверно,
// и это повод откатиться (§11.4 архитектуры).
func (s *Store) PreviousMatchRate(ctx context.Context) (float64, bool) {
	var rate float64
	err := s.pool.QueryRow(ctx, `
		SELECT match_rate FROM gar_versions
		 WHERE match_rate IS NOT NULL AND match_rate > 0
		 ORDER BY version_id DESC LIMIT 1`).Scan(&rate)
	if err != nil {
		return 0, false
	}
	return rate, true
}

/* ── Очередь расхождений ───────────────────────────────────────── */

// ReviewItem — расхождение, требующее решения человека.
type ReviewItem struct {
	Kind       string // renamed | retired | district
	AddressIDs []string
	OldValue   string
	NewValue   string
	Note       string
	GarVersion int64
}

// EnqueueReview кладёт расхождения в очередь.
//
// Сервис НЕ применяет их сам: переименование улицы в ГАР не переписывает наш
// адрес, потому что туда уже выезжала бригада. Инвариант 1 действует и при
// обновлении справочника (§11.4 архитектуры).
func (s *Store) EnqueueReview(ctx context.Context, items []ReviewItem) (int, error) {
	if len(items) == 0 {
		return 0, nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var n int
	for _, it := range items {
		// Повторная синхронизация не должна плодить дубликаты одного и того
		// же расхождения: человек и так разберёт его один раз.
		var exists bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM address_review_queue
				 WHERE kind = $1 AND old_value IS NOT DISTINCT FROM $2
				   AND new_value IS NOT DISTINCT FROM $3
				   AND resolved_at IS NULL)`,
			it.Kind, nullable(it.OldValue), nullable(it.NewValue)).Scan(&exists)
		if err != nil {
			return n, err
		}
		if exists {
			continue
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO address_review_queue
				(kind, address_ids, old_value, new_value, note, gar_version)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			it.Kind, it.AddressIDs, nullable(it.OldValue), nullable(it.NewValue),
			nullable(it.Note), it.GarVersion)
		if err != nil {
			return n, fmt.Errorf("запись расхождения: %w", err)
		}
		n++
	}

	return n, tx.Commit(ctx)
}

// ReviewQueueDepth — сколько расхождений ждёт человека. Метрика §11.6.
func (s *Store) ReviewQueueDepth(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM address_review_queue WHERE resolved_at IS NULL`).Scan(&n)
	return n, err
}

/* ── Наши адреса для сверки ────────────────────────────────────── */

// AddressRow — адрес МИС в объёме, нужном для сверки.
type AddressRow struct {
	ID       string
	Street   string
	House    string
	District string
}

// OurAddresses читает адреса МИС.
//
// Таблица addresses принадлежит МИС и до этапа 5 читается ТОЛЬКО на чтение.
// Отдельный метод, а не общий репозиторий, именно поэтому: чтобы случайная
// запись в чужую таблицу требовала осознанного действия, а не была в шаге
// от существующего кода.
func (s *Store) OurAddresses(ctx context.Context) ([]AddressRow, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, COALESCE(street, ''), COALESCE(house, ''), COALESCE(district, '')
		  FROM addresses
		 WHERE street IS NOT NULL AND street <> ''`)
	if err != nil {
		return nil, fmt.Errorf("чтение адресов: %w", err)
	}
	defer rows.Close()

	var out []AddressRow
	for rows.Next() {
		var a AddressRow
		if err := rows.Scan(&a.ID, &a.Street, &a.House, &a.District); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
