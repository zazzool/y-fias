package api

// Флаги переключения чтения (этап 4).
//
// GET отдаёт доли бэкенду МИС: он кэширует их на несколько секунд и для
// каждого запроса решает по ключу диспетчера, чей ответ показывать. PUT
// меняет долю; вызывается админской страницей МИС через её авторизованный
// маршрут, как и разбор теневых расхождений.

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/zazzool/y-fias/internal/rollout"
)

// RolloutTTL — сколько клиент может держать флаги в кэше. Это и есть время
// отката: после снижения доли все экземпляры МИС увидят его не позже чем
// через столько.
const RolloutTTL = 15 * time.Second

// RolloutResponse — текущие доли.
type RolloutResponse struct {
	Flags      map[string]int `json:"flags"`
	UpdatedAt  *time.Time     `json:"updated_at,omitempty"`
	Hash       string         `json:"hash"`
	TTLSeconds int            `json:"ttl_seconds"`
}

// RolloutSetRequest — изменение доли одного метода.
type RolloutSetRequest struct {
	Endpoint string `json:"endpoint"`
	Percent  *int   `json:"percent"`
	// Reason обязательна при увеличении доли и необязательна при откате.
	Reason string `json:"reason,omitempty"`
	Actor  string `json:"actor,omitempty"`
}

// RolloutSetResponse — итог изменения.
type RolloutSetResponse struct {
	RolloutResponse
	Change    rollout.Change `json:"change"`
	Persisted bool           `json:"persisted"`
	// Warning — изменение действует, но не сохранено и пропадёт при
	// перезапуске. Показывать администратору обязательно.
	Warning string `json:"warning,omitempty"`
}

// RolloutHistoryResponse — журнал изменений.
type RolloutHistoryResponse struct {
	Items []rollout.Change `json:"items"`
}

func (s *Server) rolloutState() RolloutResponse {
	st := s.rollout.State()
	resp := RolloutResponse{
		Flags:      st.Flags,
		Hash:       rollout.HashName,
		TTLSeconds: int(RolloutTTL / time.Second),
	}
	if !st.Updated.IsZero() {
		resp.UpdatedAt = &st.Updated
	}
	return resp
}

func (s *Server) handleRollout(w http.ResponseWriter, _ *http.Request) error {
	return writeJSON(w, http.StatusOK, s.rolloutState())
}

func (s *Server) handleRolloutSet(w http.ResponseWriter, r *http.Request) error {
	var req RolloutSetRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Percent == nil {
		// Без указателя пропущенное поле читалось бы как 0, то есть как
		// откат, о котором никто не просил.
		return badRequest("не указана доля")
	}

	// Без указанного человека в журнал идёт имя ключа: изменение без
	// автора — это раскатка, которую потом некому объяснить.
	actor := req.Actor
	if k, ok := requestKey(r); ok {
		if actor == "" {
			actor = "key:" + k.Name
		} else {
			actor += " (key:" + k.Name + ")"
		}
	}

	res, err := s.rollout.Set(r.Context(), req.Endpoint, *req.Percent, req.Reason, actor)
	if err != nil {
		if errors.Is(err, rollout.ErrUnknownEndpoint) ||
			errors.Is(err, rollout.ErrPercentRange) ||
			errors.Is(err, rollout.ErrReasonRequired) {
			return badRequest("%v", err)
		}
		return err
	}

	s.metrics.RolloutPercent.WithLabelValues(req.Endpoint).Set(float64(res.Change.To))
	if res.Change.Rollback() {
		s.metrics.RolloutRollbacks.WithLabelValues(req.Endpoint).Inc()
	}

	resp := RolloutSetResponse{
		RolloutResponse: s.rolloutState(),
		Change:          res.Change,
		Persisted:       res.Persisted,
	}
	if !res.Persisted {
		resp.Warning = fmt.Sprintf(
			"изменение действует, но не сохранено (%v): после перезапуска %s вернётся к %d%%",
			res.SaveErr, req.Endpoint, res.Change.From)
	}

	s.log.Warn("изменена доля переключения",
		"метод", req.Endpoint, "было", res.Change.From, "стало", res.Change.To,
		"кто", actor, "причина", req.Reason, "сохранено", res.Persisted)

	return writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRolloutHistory(w http.ResponseWriter, r *http.Request) error {
	if s.store == nil {
		return unavailable("журнал не подключён: история изменений недоступна")
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.store.RolloutHistory(r.Context(), limit)
	if err != nil {
		return err
	}
	if items == nil {
		items = []rollout.Change{}
	}
	return writeJSON(w, http.StatusOK, RolloutHistoryResponse{Items: items})
}
