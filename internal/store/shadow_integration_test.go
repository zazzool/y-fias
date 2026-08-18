//go:build integration

package store

import (
	"context"
	"testing"
	"time"
)

// writeShadow кладёт одно сравнение в журнал так же, как это делает
// handleShadow: через Journal, тем же набором полей meta.
func writeShadow(t *testing.T, s *Store, c struct {
	query, chosen, verdict, reason, method string
	legacyRank, oursRank                   int
	legacy, ours                           []string
}) int64 {
	t.Helper()
	ctx := context.Background()

	chosen := any(nil)
	if c.chosen != "" {
		chosen = c.chosen
	}
	method := c.method
	if method == "" {
		method = "suggest"
	}

	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO addr_events (kind, raw_input, chosen_key, meta)
		VALUES ('shadow', $1, $2, $3)
		RETURNING id`,
		c.query, chosen, map[string]any{
			"method":      method,
			"verdict":     c.verdict,
			"reason":      c.reason,
			"legacy_top":  first(c.legacy),
			"ours_top":    first(c.ours),
			"legacy_rank": c.legacyRank,
			"ours_rank":   c.oursRank,
			"legacy_list": c.legacy,
			"ours_list":   c.ours,
		}).Scan(&id)
	if err != nil {
		t.Fatalf("запись теневого события: %v", err)
	}
	return id
}

func first(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	return ss[0]
}

type shadowCase = struct {
	query, chosen, verdict, reason, method string
	legacyRank, oursRank                   int
	legacy, ours                           []string
}

func seedShadow(t *testing.T, s *Store) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DELETE FROM addr_events`); err != nil {
		t.Fatal(err)
	}

	ids := map[string]int64{}

	// Выбранное первое у нас, третье у прежней — мы лучше.
	ids["better"] = writeShadow(t, s, shadowCase{
		query: "тверск", chosen: "Тверская улица", verdict: "better",
		reason: "выбранное у нас выше", legacyRank: 2, oursRank: 0,
		legacy: []string{"Тверской бульвар", "Тверская застава", "Тверская улица"},
		ours:   []string{"Тверская улица", "Тверской бульвар"},
	})

	// Выбранного у нас нет вовсе — блокирующий класс.
	ids["worse"] = writeShadow(t, s, shadowCase{
		query: "саляма", chosen: "Саляма Адиля улица", verdict: "worse",
		reason:     "выбранного нет в нашей выдаче, а в прежней он был",
		legacyRank: 0, oursRank: -1,
		legacy: []string{"Саляма Адиля улица"},
		ours:   []string{"Салтыковская улица"},
	})

	// Тот же запрос повторно: одна причина, а не две.
	ids["worse2"] = writeShadow(t, s, shadowCase{
		query: "саляма", chosen: "Саляма Адиля улица", verdict: "worse",
		reason:     "выбранного нет в нашей выдаче, а в прежней он был",
		legacyRank: 0, oursRank: -1,
		legacy: []string{"Саляма Адиля улица"},
		ours:   []string{"Салтыковская улица"},
	})

	// Совпало, выбор известен.
	ids["same"] = writeShadow(t, s, shadowCase{
		query: "арбат", chosen: "Арбат улица", verdict: "same",
		reason: "выбранное на той же позиции", legacyRank: 0, oursRank: 0,
		legacy: []string{"Арбат улица"}, ours: []string{"Арбат улица"},
	})

	// Разбор дома: выбора человека нет, ранги отсутствуют.
	ids["house"] = writeShadow(t, s, shadowCase{
		query: "7к7", verdict: "differ", method: "house",
		reason: "канонические формы разошлись; влияет на тождество адреса",
	})

	return ids
}

func TestShadowReportCounts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedShadow(t, s)

	rep, err := s.ShadowReport(ctx, ShadowFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatalf("отчёт: %v", err)
	}

	if rep.Totals.Total != 5 || rep.Totals.Worse != 2 || rep.Totals.Better != 1 {
		t.Errorf("итоги: %+v", rep.Totals)
	}

	// Сравнение дома выбора человека не несёт и в judged попадать не должно:
	// именно на judged держится право говорить «лучше» и «хуже».
	if rep.Judged.Total != 4 {
		t.Errorf("judged = %d, ожидалось 4 (без разбора дома): %+v", rep.Judged.Total, rep.Judged)
	}

	byMethod := map[string]VerdictCounts{}
	for _, m := range rep.ByMethod {
		byMethod[m.Method] = m.Counts
	}
	if byMethod["suggest"].Total != 4 || byMethod["house"].Total != 1 {
		t.Errorf("разрез по методам: %+v", rep.ByMethod)
	}

	if len(rep.Timeline) == 0 {
		t.Error("ряд по времени пуст")
	}
}

// TestShadowReportQuality: качество считается выбором человека, а промах
// (ранг −1) даёт нулевой вклад в MRR и не участвует в средней позиции.
func TestShadowReportQuality(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedShadow(t, s)

	rep, err := s.ShadowReport(ctx, ShadowFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	q := rep.Quality
	if q.Judged != 4 {
		t.Fatalf("в расчёт качества попало %d сравнений, ожидалось 4", q.Judged)
	}

	// Наши ранги: 0, −1, −1, 0 → MRR = (1 + 0 + 0 + 1)/4 = 0.5
	if !approx(q.Ours.MRR, 0.5) {
		t.Errorf("наш MRR = %.3f, ожидалось 0.5", q.Ours.MRR)
	}
	// Прежние ранги: 2, 0, 0, 0 → MRR = (1/3 + 1 + 1 + 1)/4 = 0.8333
	if !approx(q.Legacy.MRR, 0.8333) {
		t.Errorf("MRR прежней = %.3f, ожидалось 0.833", q.Legacy.MRR)
	}
	if !approx(q.Ours.HitRate, 0.5) || !approx(q.Legacy.HitRate, 1.0) {
		t.Errorf("доля попаданий: наша %.3f, прежняя %.3f", q.Ours.HitRate, q.Legacy.HitRate)
	}
	// Средняя позиция считается только по попаданиям: у нас два попадания,
	// оба первыми.
	if !approx(q.Ours.AvgRank, 1.0) {
		t.Errorf("наша средняя позиция = %.3f, ожидалось 1.0", q.Ours.AvgRank)
	}
}

// TestShadowReportGrouping: повторяющийся запрос — одна причина, а не две.
func TestShadowReportGrouping(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedShadow(t, s)

	rep, err := s.ShadowReport(ctx, ShadowFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	if len(rep.TopWorse) != 1 {
		t.Fatalf("запросов с worse: %d, ожидался 1 сгруппированный: %+v", len(rep.TopWorse), rep.TopWorse)
	}
	if rep.TopWorse[0].Query != "саляма" || rep.TopWorse[0].Count != 2 || rep.TopWorse[0].Open != 2 {
		t.Errorf("группировка worse: %+v", rep.TopWorse[0])
	}
	if rep.LastWorse.IsZero() {
		t.Error("не заполнено время последнего блокирующего расхождения")
	}
}

func TestShadowCasesAndReview(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	ids := seedShadow(t, s)

	// Выдачи обеих систем должны доезжать до случая целиком: справочник
	// меняется, и повтор запроса ответит уже не то, что видел человек.
	cases, total, err := s.ShadowCases(ctx, ShadowFilter{
		Since: time.Now().Add(-time.Hour), Verdict: "worse",
	})
	if err != nil {
		t.Fatalf("случаи: %v", err)
	}
	if total != 2 || len(cases) != 2 {
		t.Fatalf("найдено %d случаев (total %d), ожидалось 2", len(cases), total)
	}
	c := cases[0]
	if len(c.Legacy) != 1 || c.Legacy[0] != "Саляма Адиля улица" {
		t.Errorf("выдача прежней системы не сохранилась: %+v", c.Legacy)
	}
	if len(c.Ours) != 1 || c.Ours[0] != "Салтыковская улица" {
		t.Errorf("наша выдача не сохранилась: %+v", c.Ours)
	}
	if c.OursRank != -1 || c.LegacyRank != 0 {
		t.Errorf("ранги: наш %d, прежней %d", c.OursRank, c.LegacyRank)
	}
	if c.Review != nil {
		t.Errorf("до разбора отметки быть не должно: %+v", c.Review)
	}

	// Отметка о разборе.
	err = s.ReviewShadowCase(ctx, ids["worse"], CaseReview{
		Status: "false_alarm", Note: "диспетчер выбрал не ту улицу", Reviewer: "Мухин",
	})
	if err != nil {
		t.Fatalf("отметка: %v", err)
	}

	open, _, err := s.ShadowCases(ctx, ShadowFilter{
		Since: time.Now().Add(-time.Hour), Verdict: "worse", Review: "open",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0].ID != ids["worse2"] {
		t.Errorf("после разбора неразобранным должен остаться один случай, получено %d", len(open))
	}

	// Разбор — это мнение, и оно может измениться.
	if err := s.ReviewShadowCase(ctx, ids["worse"], CaseReview{Status: "accepted", Reviewer: "Мухин"}); err != nil {
		t.Fatalf("повторная отметка: %v", err)
	}
	done, _, err := s.ShadowCases(ctx, ShadowFilter{Since: time.Now().Add(-time.Hour), Review: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 1 || done[0].Review == nil || done[0].Review.Status != "accepted" {
		t.Errorf("состояние разбора не обновилось: %+v", done)
	}

	// Сводка обязана видеть тот же разбор.
	rep, err := s.ShadowReport(ctx, ShadowFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Review.WorseTotal != 2 || rep.Review.Open != 1 || rep.Review.Accepted != 1 {
		t.Errorf("состояние разбора в сводке: %+v", rep.Review)
	}
}

// TestShadowFilterByQuery: поиск по сырому вводу — основной способ вернуться
// к конкретной жалобе диспетчера.
func TestShadowFilterByQuery(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	seedShadow(t, s)

	cases, total, err := s.ShadowCases(ctx, ShadowFilter{
		Since: time.Now().Add(-time.Hour), Query: "САЛЯМА",
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(cases) != 2 {
		t.Errorf("поиск по вводу нашёл %d (total %d), ожидалось 2", len(cases), total)
	}
}

func approx(got, want float64) bool {
	d := got - want
	return d < 0.005 && d > -0.005
}
