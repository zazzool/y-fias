//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

func TestRecordAndReadVersion(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Pool().Exec(ctx, `DELETE FROM gar_versions`); err != nil {
		t.Fatal(err)
	}

	versions := []GarVersion{
		{VersionID: 20260814, Mode: "full", Streets: 85034, Houses: 2344412, MatchRate: 0.917},
		{VersionID: 20260818, Mode: "delta", Streets: 85040, Houses: 2344500, MatchRate: 0.918},
	}
	for _, v := range versions {
		if err := s.RecordVersion(ctx, v); err != nil {
			t.Fatalf("запись версии %d: %v", v.VersionID, err)
		}
	}

	last, err := s.LastVersion(ctx)
	if err != nil {
		t.Fatalf("чтение последней версии: %v", err)
	}
	if last.VersionID != 20260818 || last.Mode != "delta" {
		t.Errorf("последняя версия %+v", last)
	}
	if last.AppliedAt.IsZero() || time.Since(last.AppliedAt) > time.Minute {
		t.Errorf("время применения не проставлено: %v", last.AppliedAt)
	}

	// Повторная запись той же версии обновляет, а не дублирует: синхронизация
	// может запуститься дважды.
	if err := s.RecordVersion(ctx, GarVersion{
		VersionID: 20260818, Mode: "delta", Streets: 99999, MatchRate: 0.9,
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM gar_versions WHERE version_id = 20260818`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("записей о версии %d, ожидалась 1", n)
	}

	rate, ok := s.PreviousMatchRate(ctx)
	if !ok || rate <= 0 {
		t.Errorf("доля совпадения не читается: %v, %v", rate, ok)
	}
}

func TestReviewQueue(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Pool().Exec(ctx, `DELETE FROM address_review_queue`); err != nil {
		t.Fatal(err)
	}

	items := []ReviewItem{
		{
			Kind:       "renamed",
			AddressIDs: []string{"a1", "a2"},
			OldValue:   "Старая улица",
			NewValue:   "Новая улица",
			Note:       "вероятное переименование",
			GarVersion: 20260818,
		},
		{
			Kind:       "district",
			AddressIDs: []string{"a3"},
			OldValue:   "Восточный",
			NewValue:   "Северное Измайлово",
			GarVersion: 20260818,
		},
	}

	n, err := s.EnqueueReview(ctx, items)
	if err != nil {
		t.Fatalf("запись расхождений: %v", err)
	}
	if n != 2 {
		t.Errorf("записано %d расхождений, ожидалось 2", n)
	}

	// Повторная синхронизация не должна плодить дубликаты: человек и так
	// разберёт расхождение один раз.
	n, err = s.EnqueueReview(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("повторная запись добавила %d расхождений, ожидалось 0", n)
	}

	depth, err := s.ReviewQueueDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != 2 {
		t.Errorf("в очереди %d расхождений, ожидалось 2", depth)
	}

	// Разобранное расхождение уходит из очереди.
	if _, err := s.Pool().Exec(ctx, `
		UPDATE address_review_queue
		   SET resolved_at = now(), resolved_by = 'тест', resolution = 'applied'
		 WHERE kind = 'district'`); err != nil {
		t.Fatal(err)
	}
	depth, _ = s.ReviewQueueDepth(ctx)
	if depth != 1 {
		t.Errorf("после разбора в очереди %d, ожидалось 1", depth)
	}

	// Массив идентификаторов должен доехать целиком: без него непонятно,
	// каких адресов касается расхождение.
	var ids []string
	err = s.Pool().QueryRow(ctx,
		`SELECT address_ids FROM address_review_queue WHERE kind = 'renamed'`).Scan(&ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a1" {
		t.Errorf("идентификаторы адресов: %v", ids)
	}
}
