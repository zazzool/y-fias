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
	"github.com/zazzool/y-fias/internal/store"
)

// Server обслуживает адресные запросы.
//
// Состояние сервера — снапшот и геометрия, оба неизменяемые. Снапшот берётся
// через Holder на каждый запрос: подмена справочника не требует ни блокировок,
// ни перезапуска (§5.1 архитектуры).
type Server struct {
	snapshots *index.Holder
	areas     *geo.Index
	coords    *geo.Coords
	log       *slog.Logger
	metrics   *Metrics
	journal   *store.Journal
	started   time.Time
}

type Options struct {
	Snapshots *index.Holder
	Areas     *geo.Index

	// Coords — координаты адресов. Необязательны: без них район выводится
	// как прежде, по соседним домам.
	Coords  *geo.Coords
	Log     *slog.Logger
	Metrics *Metrics

	// Journal — необязателен. Без него сервис работает полностью, просто не
	// накапливает материал для обучения: база не нужна для подсказок.
	Journal *store.Journal
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
		coords:    opt.Coords,
		log:       log,
		metrics:   m,
		journal:   opt.Journal,
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
	mux.HandleFunc("POST /v1/shadow", s.wrap("shadow", s.handleShadow))

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

	took := time.Since(start)

	items := make([]Suggestion, 0, len(res))
	for _, r := range res {
		items = append(items, suggestionOf(r))
		s.metrics.LayerHits.WithLabelValues(r.Layer.String()).Inc()
	}

	s.journalSuggest(req, res, snap, took)

	return writeJSON(w, http.StatusOK, SuggestResponse{
		Items: items,
		Took:  took.Round(time.Microsecond).String(),
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

	pt := geo.Point{Lon: req.Lon, Lat: req.Lat}
	source := parse.SourceUser

	// Координаты по адресу, если они не переданы напрямую.
	if pt.Lat == 0 && pt.Lon == 0 {
		if req.Street == "" {
			return badRequest("не указаны ни координаты, ни улица")
		}
		c, ok := s.lookupCoord(req)
		if !ok {
			// Честный отказ вместо догадки: без координаты район
			// определяется прежним способом, по соседним домам, и делать
			// вид, что мы знаем точно, нельзя.
			return writeJSON(w, http.StatusOK, GeoResponse{
				Confidence: "none",
				Source:     parse.SourceNone.String(),
			})
		}
		pt = geo.Point{Lon: c.Lon, Lat: c.Lat}
		source = parse.SourceOSM
	}

	area, matched := s.areas.Locate(pt)

	resp := GeoResponse{
		Matched: matched,
		Source:  parse.SourceOSM.String(),
		Lon:     pt.Lon,
		Lat:     pt.Lat,
	}
	_ = source
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

// lookupCoord ищет координату адреса.
//
// Сначала по точному дому, затем по улице целиком. Второй путь заметно менее
// надёжен: на 181 улице, пересекающей границы районов, середина улицы может
// лежать не в том районе, где стоит дом. Поэтому вызывающий обязан понижать
// уверенность, а не выдавать такой ответ как точный.
func (s *Server) lookupCoord(req GeoRequest) (geo.Coord, bool) {
	if s.coords == nil {
		return geo.Coord{}, false
	}

	key := normalize.StreetKey(req.Street)
	if key == "" {
		return geo.Coord{}, false
	}

	if req.House != "" {
		canon := parse.ParseHouse(req.House).Canonical()
		if canon != "" {
			if c, ok := s.coords.Lookup(req.District, key, canon); ok {
				return c, true
			}
			// Дом мог быть записан с другим районом. Смотрим, где он есть
			// на самом деле: расхождение района — известная беда наших
			// данных (§2.4 описи).
			if elsewhere := s.coords.LookupAnyDistrict(key, canon); len(elsewhere) == 1 {
				for _, c := range elsewhere {
					return c, true
				}
			}
		}
	}
	return geo.Coord{}, false
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
