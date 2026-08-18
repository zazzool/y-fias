// Package api — HTTP-интерфейс сервиса.
//
// Общее правило ответов: каждый несёт не только значение, но и уверенность
// (Quality) и происхождение (Source). Это прямое следствие §8.2 описи —
// система молча подменила номер дома, потому что не имела способа сказать
// «я не уверена». Клиент обязан иметь возможность отличить точное совпадение
// от догадки.
package api

import (
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/parse"
)

/* ── Общие части ───────────────────────────────────────────────── */

// Quality — коды качества в виде, пригодном для JSON.
type Quality struct {
	Parse  string `json:"parse"`            // ok | tail | garbage | ambiguous
	Street string `json:"street,omitempty"` // exact | prefix | fuzzy | none
	House  string `json:"house,omitempty"`  // in_gar | not_in_gar | unknown
	Source string `json:"source"`           // user | local | gar | osm | neighbors | ...
}

func qualityOf(q parse.Quality) Quality {
	return Quality{
		Parse:  parseQCName(q.Parse),
		Street: matchQCName(q.Street),
		House:  houseQCName(q.House),
		Source: q.Source.String(),
	}
}

func parseQCName(q parse.ParseQC) string {
	switch q {
	case parse.ParseOK:
		return "ok"
	case parse.ParseWithTail:
		return "tail"
	case parse.ParseGarbage:
		return "garbage"
	case parse.ParseAmbiguous:
		return "ambiguous"
	}
	return "?"
}

func matchQCName(q parse.MatchQC) string {
	switch q {
	case parse.MatchUnchecked:
		return ""
	case parse.MatchExact:
		return "exact"
	case parse.MatchPrefix:
		return "prefix"
	case parse.MatchFuzzy:
		return "fuzzy"
	case parse.MatchNone:
		return "none"
	}
	return ""
}

func houseQCName(q parse.HouseQC) string {
	switch q {
	case parse.HouseUnchecked:
		return ""
	case parse.HouseInGar:
		return "in_gar"
	case parse.HouseNotInGar:
		return "not_in_gar"
	case parse.HouseUnknown:
		return "unknown"
	}
	return ""
}

// Span — привязка поля к фрагменту исходной строки. Позволяет интерфейсу
// подсветить, что именно понято, а что осталось хвостом.
type Span struct {
	Field string `json:"field"`
	From  int    `json:"from"`
	To    int    `json:"to"`
}

func spansOf(in []parse.Span) []Span {
	if len(in) == 0 {
		return nil
	}
	out := make([]Span, 0, len(in))
	for _, s := range in {
		out = append(out, Span{Field: s.Field, From: s.From, To: s.To})
	}
	return out
}

/* ── suggest ───────────────────────────────────────────────────── */

type SuggestRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`

	// Region задаёт предпочтение, но НЕ фильтрует: диспетчер может искать
	// адрес области, не переключая поле региона.
	Region string `json:"region,omitempty"`
	// PatientStreets — улицы из истории пациента (§6.2 описи, чипы).
	// Сильнейший сигнал ранжирования, доступный только нам.
	PatientStreets []string `json:"patient_streets,omitempty"`
}

type SuggestResponse struct {
	Items []Suggestion `json:"items"`
	Took  string       `json:"took"`
}

// Suggestion — одна подсказка.
//
// Название улицы и её контекст отдаются РАЗДЕЛЬНО, а не склеенной строкой.
// Это тот же принцип, что и «организация рядом с адресом, а не вместо него»
// (§8.1 описи): склейку делает потребитель, зная, что показывает.
type Suggestion struct {
	Street     string `json:"street"`         // «Саляма Адиля»
	Type       string `json:"type,omitempty"` // «улица»
	Full       string `json:"full"`           // «Саляма Адиля улица»
	Label      string `json:"label"`          // с контекстом для показа
	Region     string `json:"region,omitempty"`
	District   string `json:"district,omitempty"`
	Settlement string `json:"settlement,omitempty"`

	Key   string  `json:"key"`   // ключ улицы: тождество
	Score float64 `json:"score"` // балл ранжирования
	Layer string  `json:"layer"` // exact | prefix | substring | fuzzy
	Calls uint32  `json:"calls,omitempty"`
}

func suggestionOf(r index.Result) Suggestion {
	st := r.Street
	return Suggestion{
		Street:     st.Name,
		Type:       st.Type,
		Full:       st.Full(),
		Label:      st.Label(),
		Region:     st.Region,
		District:   st.District,
		Settlement: st.Settlement,
		Key:        st.Key,
		Score:      round2(r.Score),
		Layer:      r.Layer.String(),
		Calls:      st.Calls,
	}
}

/* ── parse ─────────────────────────────────────────────────────── */

type ParseRequest struct {
	Address string `json:"address"`
	// House разбирается отдельно, если пришёл своим полем.
	House string `json:"house,omitempty"`
}

type ParseResponse struct {
	Raw        string  `json:"raw"`
	Region     string  `json:"region,omitempty"`
	District   string  `json:"district,omitempty"`
	Settlement string  `json:"settlement,omitempty"`
	Street     string  `json:"street,omitempty"`
	StreetType string  `json:"street_type,omitempty"`
	House      *House  `json:"house,omitempty"`
	Tail       string  `json:"tail,omitempty"`
	Quality    Quality `json:"quality"`
	Spans      []Span  `json:"spans,omitempty"`
}

// House — разобранный номер дома.
//
// Raw отдаётся всегда и показывается пользователю; Canonical служит ТОЛЬКО
// для установления тождества и в интерфейс попадать не должен (§8.2).
type House struct {
	Raw       string `json:"raw"`
	Canonical string `json:"canonical"`
	Num       string `json:"num,omitempty"`
	Corpus    string `json:"corpus,omitempty"`
	Building  string `json:"building,omitempty"`
	Liter     string `json:"liter,omitempty"`
	Ownership bool   `json:"ownership,omitempty"`

	// Flags — замечания к разбору. Ни одно из них не даёт права изменить
	// ввод: они основание ПРЕДЛОЖИТЬ исправление.
	Flags []string `json:"flags,omitempty"`
}

func houseOf(h parse.House) *House {
	if h.IsEmpty() && h.Raw == "" {
		return nil
	}
	return &House{
		Raw:       h.Raw,
		Canonical: h.Canonical(),
		Num:       h.Num,
		Corpus:    h.Corpus(),
		Building:  h.Building(),
		Liter:     h.Liter(),
		Ownership: h.Kind == parse.KindOwnership,
		Flags:     flagNames(h.Flags),
	}
}

func flagNames(f parse.Flags) []string {
	var out []string
	for _, x := range []struct {
		flag parse.Flags
		name string
	}{
		{parse.FlagHomoglyph, "homoglyph"},
		{parse.FlagWordNumber, "word_number"},
		{parse.FlagUnparsed, "unparsed"},
		{parse.FlagCorner, "corner"},
		{parse.FlagEmpty, "empty"},
	} {
		if f.Has(x.flag) {
			out = append(out, x.name)
		}
	}
	return out
}

/* ── validate ──────────────────────────────────────────────────── */

type ValidateRequest struct {
	Street string `json:"street"`
	House  string `json:"house,omitempty"`
	Region string `json:"region,omitempty"`
}

// ValidateResponse — результат проверки написания.
//
// Status намеренно не бинарный: «не найдено в справочнике» не равно
// «неверно». Справочник ФНС не содержит части реальных строений, а вызов
// туда всё равно приедет.
type ValidateResponse struct {
	Status string `json:"status"` // ok | warning | unknown

	// Suggestions — что предлагается вместо введённого. ПРЕДЛОЖЕНИЕ, а не
	// замена: инвариант 1 не имеет исключений.
	Suggestions []Suggestion `json:"suggestions,omitempty"`

	// Messages — пояснения для диспетчера на его языке.
	Messages []string `json:"messages,omitempty"`

	House   *House  `json:"house,omitempty"`
	Quality Quality `json:"quality"`
}

/* ── geo ───────────────────────────────────────────────────────── */

// GeoRequest — либо готовые координаты, либо адрес.
//
// Адресный вариант нужен для того, ради чего геометрия и заводилась: у
// диспетчера на руках улица и дом, а не широта с долготой.
type GeoRequest struct {
	Lon float64 `json:"lon,omitempty"`
	Lat float64 `json:"lat,omitempty"`

	Street   string `json:"street,omitempty"`
	House    string `json:"house,omitempty"`
	District string `json:"district,omitempty"` // подсказка, если район уже известен
}

type GeoResponse struct {
	District string `json:"district,omitempty"`
	Region   string `json:"region,omitempty"`

	// Lon и Lat возвращаются, когда координата найдена по адресу.
	Lon float64 `json:"lon,omitempty"`
	Lat float64 `json:"lat,omitempty"`

	// Confidence — high, если точка попала ровно в одну область.
	// Пересечение границ означает low: выбирать за человека нельзя.
	Confidence string `json:"confidence"`
	Matched    int    `json:"matched"`
	Source     string `json:"source"`
}

/* ── ошибки и служебное ────────────────────────────────────────── */

type ErrorResponse struct {
	Error string `json:"error"`
}

type HealthResponse struct {
	Status       string `json:"status"`
	SnapshotVer  int64  `json:"snapshot_version"`
	Streets      int    `json:"streets"`
	Houses       int    `json:"houses"`
	Areas        int    `json:"areas"`
	SnapshotAge  string `json:"snapshot_age"`
	WeightsVer   int    `json:"weights_version"`
	UptimeSecond int64  `json:"uptime_seconds"`
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}
