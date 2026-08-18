package api

import (
	"net/http"
	"time"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/parse"
	"github.com/zazzool/y-fias/internal/shadow"
	"github.com/zazzool/y-fias/internal/store"
)

// ShadowRequest — копия запроса из прежней системы вместе с её ответом.
//
// Сравнение делаем МЫ, а не МИС. Тогда классификация расхождений тестируема,
// а правка чужого репозитория сводится к вызову «отправь и забудь».
type ShadowRequest struct {
	// Method — что сравниваем: suggest | house.
	Method string `json:"method"`

	Query string `json:"query,omitempty"`
	House string `json:"house,omitempty"`

	// Legacy — что показала прежняя система, по порядку.
	Legacy []string `json:"legacy,omitempty"`
	// LegacyCanonical — каноническая форма номера дома из прежней системы.
	LegacyCanonical string `json:"legacy_canonical,omitempty"`

	// Chosen — что человек выбрал в итоге. Единственный объективный судья:
	// без него сравнение вырождается в спор о вкусах.
	Chosen string `json:"chosen,omitempty"`

	SessionID string `json:"session_id,omitempty"`
	Region    string `json:"region,omitempty"`
}

// ShadowResponse возвращается для отладки. В рабочем режиме МИС ответ
// игнорирует: теневой вызов не должен влиять ни на что.
type ShadowResponse struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
	OursTop string `json:"ours_top,omitempty"`
}

// handleShadow принимает пару «запрос + ответ прежней системы», считает свой
// ответ, классифицирует расхождение и кладёт его в журнал.
//
// Отвечает быстро и никогда не мешает: любая ошибка сравнения — это ошибка
// НАШЕЙ стороны, и она не должна возвращаться в МИС как проблема.
func (s *Server) handleShadow(w http.ResponseWriter, r *http.Request) error {
	var req ShadowRequest
	if err := decode(r, &req); err != nil {
		return err
	}

	snap := s.snapshots.Load()
	if snap == nil {
		return unavailable("справочник не загружен")
	}

	var cmp shadow.Comparison

	// Обе выдачи целиком уходят в журнал вместе с вердиктом. Повторить запрос
	// позже и увидеть то же самое нельзя: справочник обновляется, веса
	// меняются, и разбирать расхождение пришлось бы по памяти.
	var oursList []string

	switch req.Method {
	case "house":
		if req.House == "" {
			return badRequest("не указан номер дома")
		}
		cmp = shadow.CompareHouse(shadow.HouseCase{
			Input:           req.House,
			LegacyCanonical: req.LegacyCanonical,
			OursCanonical:   parse.ParseHouse(req.House).Canonical(),
		})

	case "suggest", "":
		if req.Query == "" {
			return badRequest("не указан запрос")
		}
		ours := snap.Suggest(index.Query{
			Text:   req.Query,
			Limit:  maxInt(len(req.Legacy), 10),
			Region: req.Region,
		})
		oursList = make([]string, 0, len(ours))
		for _, r := range ours {
			oursList = append(oursList, r.Street.Full())
		}
		cmp = shadow.CompareSuggest(shadow.SuggestCase{
			Query:  req.Query,
			Legacy: req.Legacy,
			Chosen: req.Chosen,
		}, ours)

	default:
		return badRequest("неизвестный метод сравнения %q", req.Method)
	}

	s.metrics.ShadowVerdicts.WithLabelValues(string(cmp.Verdict)).Inc()

	if s.journal != nil {
		s.journal.Write(store.Event{
			Kind:        store.EventShadow,
			SessionID:   req.SessionID,
			RawInput:    firstNonEmpty(req.Query, req.House),
			ChosenKey:   req.Chosen,
			SnapshotVer: snap.Version,
			WeightsVer:  index.DefaultWeights().Version,
			Meta: map[string]any{
				"method":      req.Method,
				"verdict":     cmp.Verdict,
				"reason":      cmp.Reason,
				"legacy_top":  cmp.LegacyTop,
				"ours_top":    cmp.OursTop,
				"legacy_rank": cmp.LegacyRank,
				"ours_rank":   cmp.OursRank,
				"legacy_list": req.Legacy,
				"ours_list":   oursList,
			},
		})
	}

	// Расхождение класса «прежняя нашла лучше» — блокирующее для этапа 4.
	// Оно должно быть заметно в логе сразу, а не всплыть в недельном отчёте.
	if cmp.Verdict == shadow.VerdictWorse {
		s.log.Warn("теневой режим: прежняя система ответила лучше",
			"запрос", cmp.Query,
			"причина", cmp.Reason,
			"прежняя", cmp.LegacyTop,
			"наша", cmp.OursTop)
	}

	return writeJSON(w, http.StatusOK, ShadowResponse{
		Verdict: string(cmp.Verdict),
		Reason:  cmp.Reason,
		OursTop: cmp.OursTop,
	})
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// journalSuggest записывает событие подсказки.
//
// Вызывается из горячего пути, поэтому обязана быть дешёвой: Write кладёт
// событие в буфер и никогда не блокирует.
func (s *Server) journalSuggest(req SuggestRequest, res []index.Result, snap *index.Snapshot, took time.Duration) {
	if s.journal == nil {
		return
	}

	keys := make([]string, 0, len(res))
	for _, r := range res {
		keys = append(keys, r.Street.Key)
	}

	s.journal.Write(store.Event{
		Kind:        store.EventSuggested,
		RawInput:    req.Query,
		Suggestions: keys,
		SnapshotVer: snap.Version,
		WeightsVer:  index.DefaultWeights().Version,
		TookMs:      float64(took.Microseconds()) / 1000,
	})
}
