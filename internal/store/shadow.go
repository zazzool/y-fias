package store

// Чтение теневого журнала.
//
// Запись (Journal) и чтение разделены намеренно: запись обязана быть дешёвой и
// работать из горячего пути, чтение обслуживает админскую страницу и может
// позволить себе агрегаты. Общее у них только имя таблицы.
//
// Ни один запрос отсюда не вызывается из пути подсказки. Если отчёт медленный,
// страдает администратор, а не диспетчер.

import (
	"context"
	"fmt"
	"strings"
	"time"
)

/* ── Значения и фильтры ────────────────────────────────────────── */

// VerdictCounts — раскладка по классам расхождений.
type VerdictCounts struct {
	Total  int `json:"total"`
	Same   int `json:"same"`
	Better int `json:"better"`
	Worse  int `json:"worse"`
	Differ int `json:"differ"`
}

func (c *VerdictCounts) add(verdict string, n int) {
	c.Total += n
	switch verdict {
	case "same":
		c.Same += n
	case "better":
		c.Better += n
	case "worse":
		c.Worse += n
	default:
		c.Differ += n
	}
}

// ShadowFilter — окно и срез. Пустые поля означают «без ограничения».
type ShadowFilter struct {
	Since   time.Time
	Method  string // suggest | house
	Verdict string // same | better | worse | differ
	// Review — состояние разбора: open (не разобрано) | done (разобрано).
	Review string
	// Query — подстрока сырого ввода, регистронезависимо.
	Query string

	Limit  int
	Offset int
}

// where собирает условия и аргументы.
//
// Значения подставляются только параметрами: сырой ввод диспетчера приходит с
// админской страницы и в SQL попадать не должен ни при каких условиях.
func (f ShadowFilter) where() (string, []any) {
	conds := []string{"e.kind = 'shadow'"}
	var args []any

	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, fmt.Sprintf(cond, len(args)))
	}

	if !f.Since.IsZero() {
		add("e.ts >= $%d", f.Since)
	}
	if f.Method != "" {
		add("e.meta->>'method' = $%d", f.Method)
	}
	if f.Verdict != "" {
		add("e.meta->>'verdict' = $%d", f.Verdict)
	}
	if f.Query != "" {
		add("e.raw_input ILIKE $%d", "%"+f.Query+"%")
	}

	switch f.Review {
	case "open":
		conds = append(conds, "(r.status IS NULL OR r.status = 'open')")
	case "done":
		conds = append(conds, "r.status IS NOT NULL AND r.status <> 'open'")
	}

	return strings.Join(conds, " AND "), args
}

/* ── Отчёт ─────────────────────────────────────────────────────── */

// ShadowReport — сводка теневого прогона.
type ShadowReport struct {
	From time.Time
	To   time.Time

	// Totals — все сравнения окна.
	Totals VerdictCounts
	// Judged — только те, где известен выбор человека. Именно они дают право
	// говорить «лучше» и «хуже»: без выбора сравнение вырождается в спор о
	// вкусах, и Y-FIAS честно отвечает differ.
	Judged VerdictCounts

	ByMethod []MethodCounts
	Timeline []TimelinePoint
	Reasons  []ReasonCount
	TopWorse []QueryCount

	Quality RankQuality
	Review  ReviewStats

	// LastWorse — когда последний раз прежняя система ответила лучше.
	// Нулевое время означает «ни разу за окно».
	LastWorse time.Time
}

type MethodCounts struct {
	Method string
	Counts VerdictCounts
}

type TimelinePoint struct {
	TS     time.Time
	Counts VerdictCounts
}

type ReasonCount struct {
	Verdict string
	Reason  string
	Count   int
}

type QueryCount struct {
	Query string
	Count int
	// Open — сколько из них ещё не разобрано.
	Open int
}

// RankQuality — качество ранжирования, измеренное выбором человека.
//
// Это тот же способ, которым измеряют поисковую выдачу: не «чей список
// красивее», а на какой позиции стоял результат, который человек выбрал.
type RankQuality struct {
	// Judged — сколько сравнений участвовало в расчёте.
	Judged int
	Legacy RankSide
	Ours   RankSide
}

// RankSide — показатели одной системы.
type RankSide struct {
	// MRR — средний обратный ранг: 1.0, если выбранное всегда было первым,
	// 0.5 — если вторым, 0 — если его не было в выдаче.
	MRR float64
	// HitRate — доля случаев, когда выбранное вообще было предложено.
	HitRate float64
	// Top1 — доля случаев, когда выбранное стояло первым.
	Top1 float64
	// AvgRank — средняя позиция выбранного (1 — первая), только по попаданиям.
	AvgRank float64
}

type ReviewStats struct {
	WorseTotal int
	Open       int
	Accepted   int
	Fixed      int
	FalseAlarm int
}

// ShadowReport собирает сводку одним обращением к базе на каждый разрез.
//
// Разрезов шесть, и они намеренно не слиты в один запрос: страница читается
// человеком, который принимает решение об этапе 4, и понятность SQL здесь
// ценнее экономии round-trip.
func (s *Store) ShadowReport(ctx context.Context, f ShadowFilter) (*ShadowReport, error) {
	rep := &ShadowReport{From: f.Since, To: time.Now()}

	where, args := f.where()
	// Все запросы отчёта считают по одному и тому же множеству событий,
	// поэтому LEFT JOIN присутствует везде: фильтр по состоянию разбора
	// обязан работать одинаково во всех разрезах.
	from := "FROM addr_events e LEFT JOIN shadow_reviews r ON r.event_id = e.id WHERE " + where

	// 1. Классы расхождений: всего, по методам и отдельно там, где известен
	//    выбор человека.
	rows, err := s.pool.Query(ctx, `
		SELECT coalesce(e.meta->>'method', 'suggest') AS method,
		       coalesce(e.meta->>'verdict', 'differ') AS verdict,
		       e.chosen_key IS NOT NULL                AS judged,
		       count(*)
		  `+from+`
		 GROUP BY 1, 2, 3`, args...)
	if err != nil {
		return nil, fmt.Errorf("сводка по классам: %w", err)
	}
	byMethod := map[string]*VerdictCounts{}
	var order []string
	for rows.Next() {
		var method, verdict string
		var judged bool
		var n int
		if err := rows.Scan(&method, &verdict, &judged, &n); err != nil {
			rows.Close()
			return nil, err
		}
		rep.Totals.add(verdict, n)
		if judged {
			rep.Judged.add(verdict, n)
		}
		if _, ok := byMethod[method]; !ok {
			byMethod[method] = &VerdictCounts{}
			order = append(order, method)
		}
		byMethod[method].add(verdict, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, m := range order {
		rep.ByMethod = append(rep.ByMethod, MethodCounts{Method: m, Counts: *byMethod[m]})
	}

	// 2. Ход во времени. Шаг выбирается по ширине окна: на суточном окне
	//    дневные столбцы бесполезны, на недельном — часовые нечитаемы.
	bucket := "day"
	if !f.Since.IsZero() && time.Since(f.Since) <= 48*time.Hour {
		bucket = "hour"
	}
	rows, err = s.pool.Query(ctx, `
		SELECT date_trunc('`+bucket+`', e.ts) AS b,
		       coalesce(e.meta->>'verdict', 'differ'),
		       count(*)
		  `+from+`
		 GROUP BY 1, 2
		 ORDER BY 1`, args...)
	if err != nil {
		return nil, fmt.Errorf("ряд по времени: %w", err)
	}
	var points []TimelinePoint
	idx := map[time.Time]int{}
	for rows.Next() {
		var b time.Time
		var verdict string
		var n int
		if err := rows.Scan(&b, &verdict, &n); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := idx[b]
		if !ok {
			points = append(points, TimelinePoint{TS: b})
			i = len(points) - 1
			idx[b] = i
		}
		points[i].Counts.add(verdict, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rep.Timeline = points

	// 3. Причины. Формулировка причины — это готовая гипотеза о том, что
	//    чинить, поэтому она показывается рядом с числом, а не прячется в
	//    карточке отдельного случая.
	rows, err = s.pool.Query(ctx, `
		SELECT coalesce(e.meta->>'verdict', 'differ'),
		       coalesce(e.meta->>'reason', ''),
		       count(*)
		  `+from+`
		 GROUP BY 1, 2
		 ORDER BY 3 DESC
		 LIMIT 40`, args...)
	if err != nil {
		return nil, fmt.Errorf("причины: %w", err)
	}
	for rows.Next() {
		var rc ReasonCount
		if err := rows.Scan(&rc.Verdict, &rc.Reason, &rc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		rep.Reasons = append(rep.Reasons, rc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 4. Качество ранжирования по выбору человека.
	//
	// Ранг −1 означает «выбранного не было в выдаче» и даёт нулевой вклад в
	// MRR — это не то же самое, что «был последним», и усреднять его как
	// позицию нельзя. Поэтому средняя позиция считается только по попаданиям.
	err = s.pool.QueryRow(ctx, `
		WITH j AS (
		    SELECT (e.meta->>'legacy_rank')::int AS lr,
		           (e.meta->>'ours_rank')::int   AS orr
		      `+from+`
		       AND e.chosen_key IS NOT NULL
		       AND e.meta->'legacy_rank' IS NOT NULL
		)
		SELECT count(*),
		       coalesce(avg(CASE WHEN lr  >= 0 THEN 1.0/(lr  + 1) ELSE 0 END), 0)::float8,
		       coalesce(avg(CASE WHEN orr >= 0 THEN 1.0/(orr + 1) ELSE 0 END), 0)::float8,
		       coalesce(avg(CASE WHEN lr  >= 0 THEN 1.0 ELSE 0 END), 0)::float8,
		       coalesce(avg(CASE WHEN orr >= 0 THEN 1.0 ELSE 0 END), 0)::float8,
		       coalesce(avg(CASE WHEN lr  = 0 THEN 1.0 ELSE 0 END), 0)::float8,
		       coalesce(avg(CASE WHEN orr = 0 THEN 1.0 ELSE 0 END), 0)::float8,
		       coalesce(avg(lr  + 1) FILTER (WHERE lr  >= 0), 0)::float8,
		       coalesce(avg(orr + 1) FILTER (WHERE orr >= 0), 0)::float8
		  FROM j`, args...).Scan(
		&rep.Quality.Judged,
		&rep.Quality.Legacy.MRR, &rep.Quality.Ours.MRR,
		&rep.Quality.Legacy.HitRate, &rep.Quality.Ours.HitRate,
		&rep.Quality.Legacy.Top1, &rep.Quality.Ours.Top1,
		&rep.Quality.Legacy.AvgRank, &rep.Quality.Ours.AvgRank,
	)
	if err != nil {
		return nil, fmt.Errorf("качество ранжирования: %w", err)
	}

	// 5. Запросы, на которых мы проигрываем. Один и тот же адрес, всплывший
	//    десять раз, — это одна причина, а не десять.
	rows, err = s.pool.Query(ctx, `
		SELECT coalesce(e.raw_input, ''),
		       count(*),
		       count(*) FILTER (WHERE r.status IS NULL OR r.status = 'open')
		  `+from+`
		   AND e.meta->>'verdict' = 'worse'
		 GROUP BY 1
		 ORDER BY 2 DESC, 1
		 LIMIT 20`, args...)
	if err != nil {
		return nil, fmt.Errorf("запросы с worse: %w", err)
	}
	for rows.Next() {
		var qc QueryCount
		if err := rows.Scan(&qc.Query, &qc.Count, &qc.Open); err != nil {
			rows.Close()
			return nil, err
		}
		rep.TopWorse = append(rep.TopWorse, qc)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 6. Состояние разбора и дата последнего блокирующего расхождения.
	err = s.pool.QueryRow(ctx, `
		SELECT count(*),
		       count(*) FILTER (WHERE r.status IS NULL OR r.status = 'open'),
		       count(*) FILTER (WHERE r.status = 'accepted'),
		       count(*) FILTER (WHERE r.status = 'fixed'),
		       count(*) FILTER (WHERE r.status = 'false_alarm'),
		       coalesce(max(e.ts), to_timestamp(0))
		  `+from+`
		   AND e.meta->>'verdict' = 'worse'`, args...).Scan(
		&rep.Review.WorseTotal, &rep.Review.Open, &rep.Review.Accepted,
		&rep.Review.Fixed, &rep.Review.FalseAlarm, &rep.LastWorse)
	if err != nil {
		return nil, fmt.Errorf("состояние разбора: %w", err)
	}
	if rep.LastWorse.Unix() == 0 {
		rep.LastWorse = time.Time{}
	}

	return rep, nil
}

/* ── Отдельные случаи ──────────────────────────────────────────── */

// ShadowCase — одно сравнение целиком, как его увидит человек.
type ShadowCase struct {
	ID     int64
	TS     time.Time
	Method string

	Verdict string
	Reason  string

	RawInput string
	Chosen   string

	LegacyTop  string
	OursTop    string
	LegacyRank int
	OursRank   int

	// Legacy и Ours — выдачи целиком, по порядку. Позволяют разобрать
	// расхождение прямо в списке, не перезапуская запрос: справочник с тех
	// пор мог измениться, и повтор ответит уже не то, что видел человек.
	Legacy []string
	Ours   []string

	Review *CaseReview
}

// CaseReview — отметка человека о разборе.
type CaseReview struct {
	Status     string
	Note       string
	Reviewer   string
	ReviewedAt time.Time
}

// ShadowCases отдаёт страницу журнала и общее число подходящих записей.
func (s *Store) ShadowCases(ctx context.Context, f ShadowFilter) ([]ShadowCase, int, error) {
	where, args := f.where()
	from := "FROM addr_events e LEFT JOIN shadow_reviews r ON r.event_id = e.id WHERE " + where

	var total int
	if err := s.pool.QueryRow(ctx, "SELECT count(*) "+from, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("число случаев: %w", err)
	}

	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	args = append(args, limit, f.Offset)

	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.ts,
		       coalesce(e.meta->>'method', 'suggest'),
		       coalesce(e.meta->>'verdict', 'differ'),
		       coalesce(e.meta->>'reason', ''),
		       coalesce(e.raw_input, ''),
		       coalesce(e.chosen_key, ''),
		       coalesce(e.meta->>'legacy_top', ''),
		       coalesce(e.meta->>'ours_top', ''),
		       coalesce((e.meta->>'legacy_rank')::int, -1),
		       coalesce((e.meta->>'ours_rank')::int, -1),
		       e.meta->'legacy_list',
		       e.meta->'ours_list',
		       r.status, r.note, r.reviewer, r.reviewed_at
		  `+from+`
		 ORDER BY e.ts DESC
		 LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("случаи: %w", err)
	}
	defer rows.Close()

	var out []ShadowCase
	for rows.Next() {
		var c ShadowCase
		var legacyList, oursList []string
		var status, note, reviewer *string
		var reviewedAt *time.Time

		if err := rows.Scan(&c.ID, &c.TS, &c.Method, &c.Verdict, &c.Reason,
			&c.RawInput, &c.Chosen, &c.LegacyTop, &c.OursTop,
			&c.LegacyRank, &c.OursRank, &legacyList, &oursList,
			&status, &note, &reviewer, &reviewedAt); err != nil {
			return nil, 0, err
		}
		c.Legacy, c.Ours = legacyList, oursList

		if status != nil {
			c.Review = &CaseReview{Status: *status}
			if note != nil {
				c.Review.Note = *note
			}
			if reviewer != nil {
				c.Review.Reviewer = *reviewer
			}
			if reviewedAt != nil {
				c.Review.ReviewedAt = *reviewedAt
			}
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ReviewShadowCase сохраняет отметку о разборе.
//
// Отметка перезаписывается: разбор — это мнение, и оно может измениться, когда
// исправление не помогло. Сам журнал при этом остаётся нетронутым.
func (s *Store) ReviewShadowCase(ctx context.Context, eventID int64, rev CaseReview) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO shadow_reviews (event_id, status, note, reviewer, reviewed_at)
		VALUES ($1, $2, nullif($3, ''), nullif($4, ''), now())
		ON CONFLICT (event_id) DO UPDATE
		   SET status = excluded.status,
		       note = excluded.note,
		       reviewer = excluded.reviewer,
		       reviewed_at = now()`,
		eventID, rev.Status, rev.Note, rev.Reviewer)
	if err != nil {
		return fmt.Errorf("отметка о разборе: %w", err)
	}
	return nil
}
