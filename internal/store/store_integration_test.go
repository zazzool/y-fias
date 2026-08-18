//go:build integration

package store

import (
	"context"
	"os"
	"testing"
	"time"
)

func testDSN() string {
	if dsn := os.Getenv("YFIAS_TEST_DSN"); dsn != "" {
		return dsn
	}
	return "postgres://yfias:yfias@localhost:55432/yfias_test?sslmode=disable"
}

func openTest(t *testing.T) *Store {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, err := Open(ctx, testDSN())
	if err != nil {
		t.Skipf("PostgreSQL недоступен (%v). Поднять: make db-up", err)
	}
	t.Cleanup(s.Close)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("миграции: %v", err)
	}
	return s
}

func TestMigrate(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	v, err := s.Version(ctx)
	if err != nil {
		t.Fatalf("версия схемы: %v", err)
	}
	if v < 1 {
		t.Errorf("версия схемы %d, ожидалась не меньше 1", v)
	}

	// Повторный накат обязан быть безвредным: миграции применяются при
	// каждом старте сервиса.
	if err := s.Migrate(ctx); err != nil {
		t.Errorf("повторный накат: %v", err)
	}

	tables := []string{
		"addr_events", "gar_versions", "address_review_queue",
		"rank_weights", "upstream_cache", "enrich_queue",
	}
	for _, tbl := range tables {
		var n int
		err := s.Pool().QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			  WHERE table_schema = 'public' AND table_name = $1`, tbl).Scan(&n)
		if err != nil {
			t.Fatalf("проверка таблицы %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("таблица %s не создана", tbl)
		}
	}
}

func TestJournalWritesEvents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if _, err := s.Pool().Exec(ctx, `DELETE FROM addr_events`); err != nil {
		t.Fatal(err)
	}

	j := NewJournal(s, JournalOptions{BatchSize: 3, FlushInterval: 200 * time.Millisecond})

	for i := range 7 {
		j.Write(Event{
			Kind:        EventSuggested,
			RawInput:    "тверская",
			Suggestions: []string{"тверская", "тверской"},
			SnapshotVer: 20260818,
			WeightsVer:  2,
			TookMs:      float64(i),
			Meta:        map[string]any{"i": i},
		})
	}
	j.Close()

	var n int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM addr_events`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 7 {
		t.Errorf("записано %d событий, ожидалось 7", n)
	}

	written, dropped := j.Stats()
	if written != 7 || dropped != 0 {
		t.Errorf("счётчики: записано %d, потеряно %d", written, dropped)
	}

	// Проверяем, что jsonb и версии доехали.
	var (
		kind string
		sugg []byte
		ver  int64
		wver int
	)
	err := s.Pool().QueryRow(ctx,
		`SELECT kind, suggestions, snapshot_ver, weights_ver
		   FROM addr_events ORDER BY id LIMIT 1`).Scan(&kind, &sugg, &ver, &wver)
	if err != nil {
		t.Fatal(err)
	}
	if kind != string(EventSuggested) || ver != 20260818 || wver != 2 {
		t.Errorf("поля события разошлись: kind=%s ver=%d wver=%d", kind, ver, wver)
	}
	if len(sugg) == 0 {
		t.Error("подсказки не записались")
	}
}

// Журнал не должен блокировать вызывающего НИКОГДА: подсказка обязана
// уложиться в 5 мс, а запись в базу от нас не зависит.
func TestJournalNeverBlocks(t *testing.T) {
	s := openTest(t)

	// Крошечный буфер: переполнение гарантировано.
	j := NewJournal(s, JournalOptions{BufferSize: 2, BatchSize: 1000,
		FlushInterval: time.Hour})
	defer j.Close()

	const n = 5000
	start := time.Now()
	for range n {
		j.Write(Event{Kind: EventTyped, RawInput: "тест"})
	}
	elapsed := time.Since(start)

	perCall := elapsed / n
	t.Logf("%d записей за %s, в среднем %s на вызов", n, elapsed.Round(time.Millisecond), perCall)

	// Запись в журнал должна стоить наносекунды, а не миллисекунды.
	if perCall > 10*time.Microsecond {
		t.Errorf("Write занимает %s на вызов — это уже влияет на горячий путь", perCall)
	}

	_, dropped := j.Stats()
	if dropped == 0 {
		t.Error("при буфере в 2 события и 5000 записях потери обязаны быть; " +
			"иначе Write где-то ждёт")
	}
	t.Logf("потеряно событий: %d (ожидаемо: буфер намеренно мал)", dropped)
}
