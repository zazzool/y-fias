//go:build integration

package normalize

import (
	"context"
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestIssue_8_5_SqlAndGoNormalizeAgree — исполняемая версия инварианта 5.
//
// §8.5 инвентаризации: список типов улиц жил в трёх местах и разошёлся, из-за
// чего результат зависел от того, кто сравнивал. В Y-FIAS источник один — Go, —
// а SQL генерируется из него. Этот тест доказывает, что генерация верна: он
// прогоняет ВСЕ 10 694 улицы прода через обе реализации и требует посимвольного
// совпадения.
//
// Запуск: make test-int (нужен docker compose up -d postgres).
func TestIssue_8_5_SqlAndGoNormalizeAgree(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, ctx)
	defer conn.Close(ctx)

	applyGeneratedSQL(t, ctx, conn)

	streets := loadStreets(t)
	t.Logf("улиц для сверки: %d", len(streets))

	// Прогон одним запросом: 10 тыс. round-trip'ов заняли бы минуты.
	rows, err := conn.Query(ctx, `
		SELECT s,
		       search_norm(s),
		       street_post_norm(s),
		       street_key(s)
		FROM unnest($1::text[]) AS s`, streets)
	if err != nil {
		t.Fatalf("запрос: %v", err)
	}
	defer rows.Close()

	var checked, mismatched int
	for rows.Next() {
		var street, sqlNorm, sqlPost, sqlKey string
		if err := rows.Scan(&street, &sqlNorm, &sqlPost, &sqlKey); err != nil {
			t.Fatalf("чтение строки: %v", err)
		}
		checked++

		if goNorm := SearchNorm(street); goNorm != sqlNorm {
			mismatched++
			if mismatched <= 10 {
				t.Errorf("SearchNorm(%q):\n  Go  = %q\n  SQL = %q", street, goNorm, sqlNorm)
			}
			continue
		}
		if goPost := postNormalize(SearchNorm(street)); goPost != sqlPost {
			mismatched++
			if mismatched <= 10 {
				t.Errorf("postNormalize(%q):\n  Go  = %q\n  SQL = %q", street, goPost, sqlPost)
			}
			continue
		}
		if goKey := StreetKey(street); goKey != sqlKey {
			mismatched++
			if mismatched <= 10 {
				t.Errorf("StreetKey(%q):\n  Go  = %q\n  SQL = %q", street, goKey, sqlKey)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("обход результата: %v", err)
	}

	if checked == 0 {
		t.Fatal("не проверено ни одной улицы")
	}
	if mismatched > 0 {
		t.Fatalf("расхождений Go и SQL: %d из %d. "+
			"Это нарушение инварианта 5: у правила нормализации должен быть "+
			"один источник. Проверьте internal/normalize/gen.", mismatched, checked)
	}
	t.Logf("сверено %d улиц, расхождений нет", checked)
}

// TestHouseNumIsPrefilterOnly фиксирует, что house_num() — предфильтр, а не
// критерий тождества (§8.2). Сама функция обязана вести себя как раньше.
func TestHouseNumIsPrefilterOnly(t *testing.T) {
	ctx := context.Background()
	conn := connect(t, ctx)
	defer conn.Close(ctx)

	applyGeneratedSQL(t, ctx, conn)

	cases := []struct {
		in   string
		want *int
	}{
		{"10к1", ptr(10)},
		{"7к7", ptr(7)},
		{"10 к 1", ptr(10)},
		{"10с4кссК", ptr(10)},
		{"вл15А", nil},
		{"СЕМЬк7", nil},
		{"", nil},
	}
	for _, c := range cases {
		var got *int
		if err := conn.QueryRow(ctx, `SELECT house_num($1)`, c.in).Scan(&got); err != nil {
			t.Fatalf("house_num(%q): %v", c.in, err)
		}
		switch {
		case c.want == nil && got != nil:
			t.Errorf("house_num(%q) = %d, ожидался NULL", c.in, *got)
		case c.want != nil && got == nil:
			t.Errorf("house_num(%q) = NULL, ожидалось %d", c.in, *c.want)
		case c.want != nil && got != nil && *c.want != *got:
			t.Errorf("house_num(%q) = %d, ожидалось %d", c.in, *got, *c.want)
		}
	}

	// Ключевое: у «7к7» и «7к1» ОДИНАКОВОЕ ведущее число. Именно поэтому оно
	// не годится как критерий тождества — на этом сломалась старая система.
	var a, b int
	_ = conn.QueryRow(ctx, `SELECT house_num('7к7'), house_num('7к1')`).Scan(&a, &b)
	if a != b {
		t.Fatalf("предпосылка теста неверна: %d != %d", a, b)
	}
}

func ptr(i int) *int { return &i }

func connect(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()

	dsn := os.Getenv("YFIAS_TEST_DSN")
	if dsn == "" {
		dsn = "postgres://yfias:yfias@localhost:55432/yfias_test?sslmode=disable"
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("PostgreSQL недоступен (%v). Поднять: make db-up", err)
	}
	return conn
}

func applyGeneratedSQL(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()

	path := filepath.Join("..", "..", "sql", "normalize.sql")
	sql, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}
	if _, err := conn.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("накат %s: %v", path, err)
	}
}

func loadStreets(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "street_keys.csv")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("golden-корпус недоступен (%v). Выгрузить: make golden", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows[1:] {
		out = append(out, row[0])
	}
	return out
}
