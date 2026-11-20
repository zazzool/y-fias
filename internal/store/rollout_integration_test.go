//go:build integration

package store

import (
	"context"
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/rollout"
)

// Доля и журнал пишутся вместе, перезапуск реестра видит сохранённое.
func TestRolloutRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.pool.Exec(ctx, `TRUNCATE rollout_flags, rollout_changes`); err != nil {
		t.Fatal(err)
	}

	r := rollout.New(nil, s)
	if _, err := r.Set(ctx, rollout.Suggest, 50, "неделя на 10% без жалоб", "admin"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond) // порядок журнала по времени
	res, err := r.Set(ctx, rollout.Suggest, 0, "", "admin")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Persisted {
		t.Fatalf("откат не сохранён: %v", res.SaveErr)
	}

	// Новый процесс с флагом запуска suggest=50: сохранённый откат важнее.
	restarted := rollout.New(map[string]int{rollout.Suggest: 50}, s)
	if err := restarted.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if p := restarted.Percent(rollout.Suggest); p != 0 {
		t.Errorf("после перезапуска suggest = %d%%, ожидался сохранённый 0", p)
	}

	hist, err := s.RolloutHistory(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("в журнале %d записей, ожидалось 2", len(hist))
	}
	if !hist[0].Rollback() || hist[0].From != 50 || hist[0].To != 0 {
		t.Errorf("последней должна быть запись отката: %+v", hist[0])
	}
	if hist[1].Reason != "неделя на 10% без жалоб" || hist[1].Actor != "admin" {
		t.Errorf("причина или автор потеряны: %+v", hist[1])
	}
}
