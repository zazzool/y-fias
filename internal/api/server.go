package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/zazzool/y-fias/internal/geo"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
	"github.com/zazzool/y-fias/internal/parse"
)

// Server обслуживает адресные запросы.
//
// Состояние сервера — снапшот и геометрия, оба неизменяемые. Снапшот берётся
// через Holder на каждый запрос: подмена справочника не требует ни блокировок,
// ни перезапуска (§5.1 архитектуры).
type Server struct {
	snapshots *index.Holder
	areas     *geo.Index
	log       *slog.Logger
	metrics   *Metrics
	started   time.Time
}

type Options struct {
	Snapshots *index.Holder
	Areas     *geo.Index
	Log       *slog.Logger
	Metrics   *Metrics
}

func New(opt Options) *Server {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	m := opt.Metrics
	if m == nil {
		m = NewMetrics()
	}
	return &Server{
		snapshots: opt.Snapshots,
		areas:     opt.Areas,
		log:       log,
		metrics:   m,
		started:   time.Now(),
	}
}

// Handler собирает маршруты.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/suggest", s.wrap("suggest", s.handleSuggest))
	mux.HandleFunc("POST /v1/parse", s.wrap("parse", s.handleParse))
	mux.HandleFunc("POST /v1/validate", s.wrap("validate", s.handleValidate))
	mux.HandleFunc("POST /v1/geo", s.wrap("geo", s.handleGeo))

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.Handle("GET /metrics", s.metrics.Handler())

	return mux
}

// wrap добавляет к обработчику измерение времени, журнал и восстановление
// после паники.
//
// Паника в одном запросе не должна ронять сервис: диспетчерская работает
// круглосуточно, и падение процесса из-за одного кривого ввода недопустимо.
func (s *Server) wrap(name string, fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		defer func() {
			if rec := recover(); rec != nil {
				s.metrics.Panics.Inc()
				s.log.Error("паника в обработчике",
					"метод", name, "причина", rec)
				writeJSON(w, http.StatusInternalServerError,
					ErrorResponse{Error: "внутренняя ошибка"})
			}
		}()

		err := fn(w, r)
		took := time.Since(start)
		s.metrics.observe(name, took, err)

		if err != nil {
			status := http.StatusBadRequest
			var he httpError
			if errors.As(err, &he) {
				status = he.status
			}
			s.log.Warn("ошибка запроса", "метод", name, "ошибка", err)
			writeJSON(w, status, ErrorResponse{Error: err.Error()})
		}
	}
}

type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return httpError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, args...)}
}

func unavailable(format string, args ...any) error {
	return httpError{status: http.StatusServiceUnavailable, msg: fmt.Sprintf(format, args...)}
}

/* ── suggest ───────────────────────────────────────────────────── */

func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request) error {
	var req SuggestRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Query == "" {
		return badRequest("пустой запрос")
	}

	snap := s.snapshots.Load()
	if snap == nil {
		return unavailable("справочник не загружен")
	}

	start := time.Now()
	res := snap.Suggest(index.Query{
		Text:           req.Query,
		Limit:          req.Limit,
		Region:         req.Region,
		PatientStreets: resolvePatientStreets(snap, req.PatientStreets),
	})

	items := make([]Suggestion, 0, len(res))
	for _, r := range res {
		items = append(items, suggestionOf(r))
		s.metrics.LayerHits.WithLabelValues(r.Layer.String()).Inc()
	}

	return writeJSON(w, http.StatusOK, SuggestResponse{
		Items: items,
		Took:  time.Since(start).Round(time.Microsecond).String(),
	})
}

// resolvePatientStreets переводит названия улиц пациента в позиции снапшота.
//
// Клиент присылает названия, а не идентификаторы: у него их и нет, а
// идентификаторы снапшота меняются при каждой пересборке и наружу отдаваться
// не должны.
func resolvePatientStreets(snap *index.Snapshot, names []string) []uint32 {
	if len(names) == 0 {
		return nil
	}
	out := make([]uint32, 0, len(names))
	for _, n := range names {
		for _, r := range snap.Suggest(index.Query{Text: n, Limit: 1}) {
			if r.Layer == index.LayerExact {
				out = append(out, r.Street.ID)
			}
		}
	}
	return out
}

/* ── parse ─────────────────────────────────────────────────────── */

func (s *Server) handleParse(w http.ResponseWriter, r *http.Request) error {
	var req ParseRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Address == "" && req.House == "" {
		return badRequest("не указан адрес")
	}

	a := parse.ParseAddress(req.Address)

	// Дом, пришедший отдельным полем, важнее выделенного из строки:
	// в форме карты вызова он и есть отдельное поле.
	if req.House != "" {
		a.House = parse.ParseHouse(req.House)
	}

	return writeJSON(w, http.StatusOK, ParseResponse{
		Raw:        a.Raw,
		Region:     a.Region,
		District:   a.District,
		Settlement: a.Settlement,
		Street:     a.Street,
		StreetType: a.StreetType,
		House:      houseOf(a.House),
		Tail:       a.Tail,
		Quality:    qualityOf(a.Quality),
		Spans:      spansOf(a.Spans),
	})
}

/* ── validate ──────────────────────────────────────────────────── */

// handleValidate проверяет написание и предлагает исправления.
//
// Эндпоинт существует и в старой системе, но SPA его не вызывает (§8.8
// описи): диспетчер не видит предупреждений об опечатках и латинской
// раскладке. Включение его в интерфейс — готовая ценность этапа 4.
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) error {
	var req ValidateRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if req.Street == "" {
		return badRequest("не указана улица")
	}

	snap := s.snapshots.Load()
	if snap == nil {
		return unavailable("справочник не загружен")
	}

	resp := ValidateResponse{Status: "unknown"}
	resp.Quality.Source = parse.SourceUser.String()

	// Латинская раскладка. Автозамены нет — только предложение.
	if parse.IsLikelyWrongLayout(req.Street) {
		fixed := parse.LatinToCyrillicLayout(req.Street)
		resp.Messages = append(resp.Messages,
			fmt.Sprintf("похоже на латинскую раскладку, возможно: «%s»", fixed))
		req.Street = fixed
	}

	if req.House != "" {
		h := parse.ParseHouse(req.House)
		resp.House = houseOf(h)
		resp.Messages = append(resp.Messages, houseMessages(h)...)
	}

	res := snap.Suggest(index.Query{Text: req.Street, Limit: 5, Region: req.Region})
	if len(res) == 0 {
		resp.Messages = append(resp.Messages, "улица не найдена в справочнике")
		resp.Quality.Street = matchQCName(parse.MatchNone)
		return writeJSON(w, http.StatusOK, resp)
	}

	top := res[0]
	switch top.Layer {
	case index.LayerExact:
		resp.Status = "ok"
		resp.Quality.Street = matchQCName(parse.MatchExact)
	case index.LayerPrefix, index.LayerSubstring:
		resp.Status = "warning"
		resp.Quality.Street = matchQCName(parse.MatchPrefix)
		resp.Messages = append(resp.Messages, "улица найдена не точно, проверьте написание")
	default:
		resp.Status = "warning"
		resp.Quality.Street = matchQCName(parse.MatchFuzzy)
		resp.Messages = append(resp.Messages,
			fmt.Sprintf("возможно, имелась в виду «%s»", top.Street.Full()))
	}

	for _, r := range res {
		resp.Suggestions = append(resp.Suggestions, suggestionOf(r))
	}

	// Существование дома проверяется только при точном совпадении улицы:
	// иначе мы сверялись бы с домами не той улицы.
	if req.House != "" && top.Layer == index.LayerExact {
		qc := snap.LookupHouse(top.Street.ID, parse.ParseHouse(req.House))
		resp.Quality.House = houseQCName(qc)
		if qc == parse.HouseNotInGar {
			// НЕ ошибка: справочник ФНС не содержит части реальных
			// строений (§8.7 описи), а вызов туда всё равно приедет.
			resp.Messages = append(resp.Messages,
				"дома нет в справочнике ГАР; это не ошибка, но проверьте номер")
		}
	}

	return writeJSON(w, http.StatusOK, resp)
}

func houseMessages(h parse.House) []string {
	var out []string
	if h.Flags.Has(parse.FlagHomoglyph) {
		out = append(out, fmt.Sprintf(
			"в номере дома латиница или похожие символы, возможно: «%s»", h.Canonical()))
	}
	if h.Flags.Has(parse.FlagWordNumber) {
		out = append(out, "номер дома записан прописью")
	}
	if h.Flags.Has(parse.FlagUnparsed) {
		out = append(out, "в номере дома есть лишний текст; корпус и отделение вносятся в примечания")
	}
	if h.Flags.Has(parse.FlagEmpty) {
		out = append(out, "номер дома не распознан")
	}
	return out
}

/* ── geo ───────────────────────────────────────────────────────── */

func (s *Server) handleGeo(w http.ResponseWriter, r *http.Request) error {
	var req GeoRequest
	if err := decode(r, &req); err != nil {
		return err
	}
	if s.areas == nil {
		return unavailable("границы районов не загружены")
	}
	if req.Lat == 0 && req.Lon == 0 {
		return badRequest("не указаны координаты")
	}

	area, matched := s.areas.Locate(geo.Point{Lon: req.Lon, Lat: req.Lat})

	resp := GeoResponse{Matched: matched, Source: parse.SourceOSM.String()}
	switch {
	case area == nil:
		resp.Confidence = "none"
	case matched == 1:
		resp.District, resp.Region = area.Name, area.Region
		resp.Confidence = "high"
	default:
		// Границы перекрываются: выбирать за человека нельзя.
		resp.District, resp.Region = area.Name, area.Region
		resp.Confidence = "low"
	}
	return writeJSON(w, http.StatusOK, resp)
}

/* ── служебные ─────────────────────────────────────────────────── */

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:       "ok",
		UptimeSecond: int64(time.Since(s.started).Seconds()),
		WeightsVer:   index.DefaultWeights().Version,
	}
	if snap := s.snapshots.Load(); snap != nil {
		resp.SnapshotVer = snap.Version
		resp.Streets = snap.Len()
		resp.Houses = snap.HouseCount()
		resp.SnapshotAge = s.snapshots.Age().Round(time.Second).String()
	}
	if s.areas != nil {
		resp.Areas = s.areas.Len()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleReady сообщает, готов ли сервис обслуживать запросы.
//
// Отделено от healthz намеренно: процесс может быть жив, но ещё не загрузить
// справочник, и балансировщик не должен слать на него запросы.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	snap := s.snapshots.Load()
	if snap == nil || snap.Len() == 0 {
		writeJSON(w, http.StatusServiceUnavailable,
			ErrorResponse{Error: "справочник не загружен"})
		return
	}
	writeJSON(w, http.StatusOK, HealthResponse{Status: "ready", Streets: snap.Len()})
}

/* ── вспомогательное ───────────────────────────────────────────── */

// maxBody ограничивает размер запроса: адресная строка не бывает длиннее
// килобайта, а без ограничения один запрос способен исчерпать память.
const maxBody = 64 << 10

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("не удалось разобрать запрос: %v", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	// Кириллица должна читаться в логах и curl как есть, а не как \uXXXX.
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Ключ улицы наружу отдаётся как есть — он детерминирован и служит клиенту
// для сопоставления, а не для показа.
var _ = normalize.StreetKey
