package api

// Чтение теневого журнала для админской страницы МИС.
//
// Отдельно от handleShadow намеренно: тот принимает данные из горячего пути и
// обязан быть дешёвым, эти — обслуживают человека, который решает, можно ли
// переходить к этапу 4. Разные требования, разная цена ошибки.
//
// Наружу отдаётся ровно то, что нужно для решения, и ничего сверх: сырой ввод
// диспетчера — медицински чувствительная строка, и её незачем разглашать
// кому-то кроме администратора МИС (доступ проверяет МИС, проксируя запрос).

import (
	"net/http"
	"strconv"
	"time"

	"github.com/zazzool/y-fias/internal/store"
)

/* ── Отчёт ─────────────────────────────────────────────────────── */

type VerdictCountsDTO struct {
	Total  int `json:"total"`
	Same   int `json:"same"`
	Better int `json:"better"`
	Worse  int `json:"worse"`
	Differ int `json:"differ"`
}

func countsOf(c store.VerdictCounts) VerdictCountsDTO {
	return VerdictCountsDTO{Total: c.Total, Same: c.Same, Better: c.Better, Worse: c.Worse, Differ: c.Differ}
}

type ShadowReportResponse struct {
	From string `json:"from"`
	To   string `json:"to"`
	Days int    `json:"days"`

	Totals VerdictCountsDTO `json:"totals"`
	Judged VerdictCountsDTO `json:"judged"`

	ByMethod []MethodCountsDTO  `json:"by_method"`
	Timeline []TimelinePointDTO `json:"timeline"`
	Reasons  []ReasonCountDTO   `json:"reasons"`
	TopWorse []QueryCountDTO    `json:"top_worse"`
	Quality  RankQualityDTO     `json:"quality"`
	Review   ReviewStatsDTO     `json:"review"`

	// Gate — состояние условия выхода этапа 3. Оно одно: неразобранных
	// расхождений класса worse должно быть ноль.
	Gate GateDTO `json:"gate"`
}

type MethodCountsDTO struct {
	Method string           `json:"method"`
	Counts VerdictCountsDTO `json:"counts"`
}

type TimelinePointDTO struct {
	TS     string           `json:"ts"`
	Counts VerdictCountsDTO `json:"counts"`
}

type ReasonCountDTO struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	Count   int    `json:"count"`
}

type QueryCountDTO struct {
	Query string `json:"query"`
	Count int    `json:"count"`
	Open  int    `json:"open"`
}

type RankSideDTO struct {
	MRR     float64 `json:"mrr"`
	HitRate float64 `json:"hit_rate"`
	Top1    float64 `json:"top1"`
	AvgRank float64 `json:"avg_rank"`
}

type RankQualityDTO struct {
	Judged int         `json:"judged"`
	Legacy RankSideDTO `json:"legacy"`
	Ours   RankSideDTO `json:"ours"`
}

type ReviewStatsDTO struct {
	WorseTotal int `json:"worse_total"`
	Open       int `json:"open"`
	Accepted   int `json:"accepted"`
	Fixed      int `json:"fixed"`
	FalseAlarm int `json:"false_alarm"`
}

// GateDTO — прямой ответ на вопрос «можно ли переходить к этапу 4».
//
// Считается здесь, а не в интерфейсе: условие выхода — часть договорённости о
// сервисе, и оно должно быть одинаковым в отчёте, в логе и на странице.
type GateDTO struct {
	Ready bool `json:"ready"`
	// OpenWorse — неразобранные блокирующие расхождения.
	OpenWorse int `json:"open_worse"`
	// CleanHours — сколько часов прошло с последнего worse. Ноль означает,
	// что за окно их не было вовсе (см. HadWorse).
	CleanHours float64 `json:"clean_hours"`
	HadWorse   bool    `json:"had_worse"`
	// Sample — сколько сравнений накоплено. Ноль расхождений на пустой
	// выборке не значит ничего, и страница обязана это показывать.
	Sample int `json:"sample"`
	// Judged — из них с известным выбором человека.
	Judged int    `json:"judged"`
	Reason string `json:"reason"`
}

func (s *Server) handleShadowReport(w http.ResponseWriter, r *http.Request) error {
	if s.store == nil {
		return unavailable("журнал не подключён: отчёт недоступен")
	}

	f, days := shadowFilter(r)
	rep, err := s.store.ShadowReport(r.Context(), f)
	if err != nil {
		return err
	}

	resp := ShadowReportResponse{
		From:   rep.From.Format(time.RFC3339),
		To:     rep.To.Format(time.RFC3339),
		Days:   days,
		Totals: countsOf(rep.Totals),
		Judged: countsOf(rep.Judged),
		Quality: RankQualityDTO{
			Judged: rep.Quality.Judged,
			Legacy: sideOf(rep.Quality.Legacy),
			Ours:   sideOf(rep.Quality.Ours),
		},
		Review: ReviewStatsDTO{
			WorseTotal: rep.Review.WorseTotal,
			Open:       rep.Review.Open,
			Accepted:   rep.Review.Accepted,
			Fixed:      rep.Review.Fixed,
			FalseAlarm: rep.Review.FalseAlarm,
		},
	}

	for _, m := range rep.ByMethod {
		resp.ByMethod = append(resp.ByMethod, MethodCountsDTO{Method: m.Method, Counts: countsOf(m.Counts)})
	}
	for _, p := range rep.Timeline {
		resp.Timeline = append(resp.Timeline, TimelinePointDTO{
			TS: p.TS.Format(time.RFC3339), Counts: countsOf(p.Counts),
		})
	}
	for _, x := range rep.Reasons {
		resp.Reasons = append(resp.Reasons, ReasonCountDTO{Verdict: x.Verdict, Reason: x.Reason, Count: x.Count})
	}
	for _, x := range rep.TopWorse {
		resp.TopWorse = append(resp.TopWorse, QueryCountDTO{Query: x.Query, Count: x.Count, Open: x.Open})
	}

	resp.Gate = gateOf(rep)
	return writeJSON(w, http.StatusOK, resp)
}

func sideOf(s store.RankSide) RankSideDTO {
	return RankSideDTO{
		MRR:     round3(s.MRR),
		HitRate: round3(s.HitRate),
		Top1:    round3(s.Top1),
		AvgRank: round3(s.AvgRank),
	}
}

// gateOf формулирует вердикт по этапу целиком.
//
// «Ноль worse» на трёх сравнениях — это не готовность, а отсутствие данных.
// Поэтому в условие входит и объём выборки: неделя реальной нагрузки, о
// которой говорит план, — это тысячи сравнений, а не десятки.
func gateOf(rep *store.ShadowReport) GateDTO {
	const minSample = 500

	g := GateDTO{
		OpenWorse: rep.Review.Open,
		HadWorse:  !rep.LastWorse.IsZero(),
		Sample:    rep.Totals.Total,
		Judged:    rep.Judged.Total,
	}
	if g.HadWorse {
		g.CleanHours = round3(time.Since(rep.LastWorse).Hours())
	}

	switch {
	case g.Sample < minSample:
		g.Reason = "данных мало: ноль расхождений на такой выборке ничего не доказывает"
	case g.OpenWorse > 0:
		g.Reason = "есть неразобранные расхождения класса «прежняя ответила лучше»"
	case g.Judged == 0:
		g.Reason = "ни одного сравнения с выбором человека: судить не о чем"
	default:
		g.Ready = true
		g.Reason = "блокирующих расхождений нет, все разобраны"
	}
	return g
}

/* ── Отдельные случаи ──────────────────────────────────────────── */

type ShadowCaseDTO struct {
	ID     int64  `json:"id"`
	TS     string `json:"ts"`
	Method string `json:"method"`

	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`

	Query  string `json:"query"`
	Chosen string `json:"chosen,omitempty"`

	LegacyTop  string `json:"legacy_top,omitempty"`
	OursTop    string `json:"ours_top,omitempty"`
	LegacyRank int    `json:"legacy_rank"`
	OursRank   int    `json:"ours_rank"`

	Legacy []string `json:"legacy,omitempty"`
	Ours   []string `json:"ours,omitempty"`

	Review *CaseReviewDTO `json:"review,omitempty"`
}

type CaseReviewDTO struct {
	Status     string `json:"status"`
	Note       string `json:"note,omitempty"`
	Reviewer   string `json:"reviewer,omitempty"`
	ReviewedAt string `json:"reviewed_at,omitempty"`
}

type ShadowCasesResponse struct {
	Items  []ShadowCaseDTO `json:"items"`
	Total  int             `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

func (s *Server) handleShadowCases(w http.ResponseWriter, r *http.Request) error {
	if s.store == nil {
		return unavailable("журнал не подключён: список расхождений недоступен")
	}

	f, _ := shadowFilter(r)
	f.Limit = atoiDefault(r.URL.Query().Get("limit"), 50)
	f.Offset = atoiDefault(r.URL.Query().Get("offset"), 0)

	cases, total, err := s.store.ShadowCases(r.Context(), f)
	if err != nil {
		return err
	}

	resp := ShadowCasesResponse{
		Items:  make([]ShadowCaseDTO, 0, len(cases)),
		Total:  total,
		Limit:  f.Limit,
		Offset: f.Offset,
	}
	for _, c := range cases {
		dto := ShadowCaseDTO{
			ID: c.ID, TS: c.TS.Format(time.RFC3339), Method: c.Method,
			Verdict: c.Verdict, Reason: c.Reason,
			Query: c.RawInput, Chosen: c.Chosen,
			LegacyTop: c.LegacyTop, OursTop: c.OursTop,
			LegacyRank: c.LegacyRank, OursRank: c.OursRank,
			Legacy: c.Legacy, Ours: c.Ours,
		}
		if c.Review != nil {
			dto.Review = &CaseReviewDTO{
				Status:     c.Review.Status,
				Note:       c.Review.Note,
				Reviewer:   c.Review.Reviewer,
				ReviewedAt: c.Review.ReviewedAt.Format(time.RFC3339),
			}
		}
		resp.Items = append(resp.Items, dto)
	}
	return writeJSON(w, http.StatusOK, resp)
}

/* ── Отметка о разборе ─────────────────────────────────────────── */

type ShadowReviewRequest struct {
	EventID int64  `json:"event_id"`
	Status  string `json:"status"`
	Note    string `json:"note,omitempty"`
	// Reviewer — кто разобрал. Передаёт МИС: собственных учётных записей у
	// сервиса нет и заводить их незачем.
	Reviewer string `json:"reviewer,omitempty"`
}

var reviewStatuses = map[string]bool{
	"open": true, "accepted": true, "fixed": true, "false_alarm": true,
}

func (s *Server) handleShadowReview(w http.ResponseWriter, r *http.Request) error {
	if s.store == nil {
		return unavailable("журнал не подключён: отметка не сохранится")
	}

	var req ShadowReviewRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.EventID <= 0 {
		return badRequest("не указан случай")
	}
	if !reviewStatuses[req.Status] {
		return badRequest("неизвестное состояние разбора %q", req.Status)
	}

	err := s.store.ReviewShadowCase(r.Context(), req.EventID, store.CaseReview{
		Status:   req.Status,
		Note:     req.Note,
		Reviewer: req.Reviewer,
	})
	if err != nil {
		return err
	}
	return writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

/* ── Разбор параметров ─────────────────────────────────────────── */

// shadowFilter читает окно и срез из строки запроса.
//
// Значения, которых сервис не понимает, молча игнорируются, а не приводят к
// ошибке: страница отчёта не должна ломаться из-за устаревшей закладки.
func shadowFilter(r *http.Request) (store.ShadowFilter, int) {
	q := r.URL.Query()

	days := atoiDefault(q.Get("days"), 7)
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}

	f := store.ShadowFilter{
		Since: time.Now().Add(-time.Duration(days) * 24 * time.Hour),
		Query: q.Get("q"),
	}
	switch m := q.Get("method"); m {
	case "suggest", "house":
		f.Method = m
	}
	switch v := q.Get("verdict"); v {
	case "same", "better", "worse", "differ":
		f.Verdict = v
	}
	switch rv := q.Get("review"); rv {
	case "open", "done":
		f.Review = rv
	}
	return f, days
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func round3(f float64) float64 {
	return float64(int64(f*1000+0.5)) / 1000
}
