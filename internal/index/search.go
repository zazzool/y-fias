package index

import (
	"math"
	"sort"
	"strings"

	"github.com/zazzool/y-fias/internal/normalize"
)

// Layer — слой, на котором найдено совпадение. Отдаётся наружу: интерфейс
// может показать, насколько уверенно найдено, а метрики — распределение по
// слоям (suggest_layer_hit).
type Layer uint8

const (
	LayerExact Layer = iota
	LayerPrefix
	LayerSubstring
	LayerFuzzy
)

func (l Layer) String() string {
	switch l {
	case LayerExact:
		return "exact"
	case LayerPrefix:
		return "prefix"
	case LayerSubstring:
		return "substring"
	case LayerFuzzy:
		return "fuzzy"
	}
	return "?"
}

// Базовые баллы слоёв. Перенесены из streetDictionary.ts.
const (
	scoreExact      = 100.0
	scorePrefix     = 80.0
	scoreWordPrefix = 70.0 // совпадение с внутренним словом названия
	scoreSubstring  = 60.0
	scoreFuzzy      = 30.0
	scoreFuzzySpan  = 30.0 // добавка, пропорциональная сходству
)

// Weights — веса ранжирования.
//
// Не константы в коде, а значения с версией: их подбирают офлайн на
// golden-корпусе по метрике «букв до правильной подсказки»
// (cmd/yfias-replay), и версия должна попадать в журнал событий, чтобы
// откат весов откатывал и выученные на них правила (§8.3 архитектуры).
type Weights struct {
	Version int

	// Calls — вес частоты вызовов. Главное преимущество перед универсальными
	// сервисами: они такого сигнала не имеют.
	Calls float64
	// Region — надбавка за регион 77 (10 649 адресов из 10 694).
	Region float64
	// Type — надбавка за совпавший тип улицы.
	Type float64
	// Patient — надбавка за адрес из истории пациента.
	Patient float64
	// Settlement — надбавка за совпавший населённый пункт.
	Settlement float64
	// Coverage — надбавка за долю названия, покрытую запросом.
	//
	// Если введено «саляма адиля», а кандидат называется именно так, это
	// вероятнее, чем кандидат, где введённое — лишь малая часть названия.
	// Сигнал сильный и дешёвый: он не требует ни данных, ни обучения.
	Coverage float64
	// ShortName — надбавка за короткое название при коротком запросе.
	// Разводит кандидатов там, где покрытие у всех одинаково мало.
	ShortName float64
}

// DefaultWeights — стартовые значения до подбора на корпусе.
func DefaultWeights() Weights {
	return Weights{
		Version:    1,
		Calls:      6.0,
		Region:     12.0,
		Type:       5.0,
		Patient:    25.0,
		Settlement: 8.0,
		Coverage:   10.0,
		ShortName:  4.0,
	}
}

// Query — запрос подсказки.
type Query struct {
	Text  string
	Limit int

	// Region задаёт предпочтение, но НЕ фильтрует: диспетчер может искать
	// адрес области, не переключая поле региона.
	Region string
	// Settlement — контекст населённого пункта, если он уже известен.
	Settlement string
	// PatientStreets — улицы из истории пациента (§6.2 описи, чипы).
	PatientStreets []uint32

	Weights Weights
}

// Result — одна подсказка.
type Result struct {
	Street *Street
	Score  float64
	Layer  Layer
}

const defaultLimit = 10

// Suggest ищет подсказки.
//
// Слои делятся на дешёвые и дорогие. Точный (карта) и префиксный (бинарный
// поиск) выполняются всегда — их стоимость не зависит от размера справочника.
// Подстрочный (линейный проход) и нечёткий (расстояния на сотнях кандидатов)
// выполняются, только если дешёвые не нашли НИЧЕГО.
//
// Именно это удерживает p99: обычный ввод обслуживается за единицы микросекунд,
// а дорогие слои достаются лишь запросам, которые иначе остались бы без ответа.
func (s *Snapshot) Suggest(q Query) []Result {
	if q.Limit <= 0 {
		q.Limit = defaultLimit
	}
	if q.Weights.Version == 0 {
		q.Weights = DefaultWeights()
	}

	text := normalize.SearchNorm(q.Text)
	if text == "" {
		return nil
	}
	key := normalize.StreetKey(q.Text)

	acc := &accumulator{
		seen:   make(map[uint32]int, q.Limit*4),
		query:  q,
		snap:   s,
		qRunes: len([]rune(text)),
	}

	// Слои 1 и 2 — дешёвые, выполняются всегда.
	//
	// Слой 1: точное совпадение ключа, O(1) по карте.
	for _, id := range s.byKey[key] {
		acc.add(id, scoreExact, LayerExact)
	}

	// Слой 2: префикс. Диапазон в отсортированном массиве находится бинарным
	// поиском, поэтому стоимость O(log n) независимо от размера справочника.
	// Индексируются и полное название, и имя без типа, и КАЖДОЕ СЛОВО имени:
	// диспетчер вводит «адиля», имея в виду «Саляма Адиля улица».
	lo := sort.SearchStrings(s.prefixKeys, text)
	for i := lo; i < len(s.prefixKeys); i++ {
		if !strings.HasPrefix(s.prefixKeys[i], text) {
			break
		}
		score := scorePrefix
		if s.prefixInner[i] {
			score = scoreWordPrefix
		}
		acc.add(s.prefixIDs[i], score, LayerPrefix)
	}

	// Дальше идут ДОРОГИЕ слои: подстрочный — линейный проход по всему
	// справочнику, нечёткий — расстояния на сотнях кандидатов. Они нужны
	// только когда дешёвые не нашли ничего.
	//
	// Условием выхода раньше было «набрано Limit результатов», и это была
	// ошибка: точное совпадение даёт ОДИН результат, поэтому самый частый и
	// самый однозначный запрос всё равно проходил все четыре слоя и оказывался
	// медленнее худшего случая — 514 мкс против 342 мкс на замере.
	if len(acc.results) > 0 {
		return acc.top()
	}

	// Слой 3: подстрока. Линейный проход; на 85 тыс. объектов ГАР это уже
	// заметно, поэтому сюда доходят только запросы, ничего не давшие выше.
	for i := range s.streets {
		if strings.Contains(s.streets[i].Norm, text) {
			acc.add(uint32(i), scoreSubstring, LayerSubstring)
		}
	}
	if len(acc.results) > 0 {
		return acc.top()
	}

	// Слой 4: нечёткое совпадение. Кандидаты отбираются по общим биграммам,
	// и только на них считается расстояние — иначе пришлось бы сравнивать
	// запрос с каждой из десятков тысяч улиц.
	for _, id := range s.fuzzyCandidates(text, q.Limit*8) {
		ok, sim := IsSimilar(text, s.streets[id].Norm)
		if !ok {
			// Второй шанс: сравнение с названием без типа. Диспетчер
			// печатает «тверска», а Norm содержит «тверская улица».
			ok, sim = IsSimilar(text, normalize.SearchNorm(s.streets[id].Name))
		}
		if ok {
			acc.add(id, scoreFuzzy+sim*scoreFuzzySpan, LayerFuzzy)
		}
	}

	return acc.top()
}

// fuzzyCandidates отбирает объекты, у которых больше всего общих биграмм с
// запросом. Возвращает не более limit позиций.
func (s *Snapshot) fuzzyCandidates(text string, limit int) []uint32 {
	grams := bigramsOf(text)
	if len(grams) == 0 {
		return nil
	}

	hits := make(map[uint32]int, limit*4)
	seen := make(map[uint32]bool, len(grams))
	for _, g := range grams {
		if seen[g] {
			continue
		}
		seen[g] = true
		for _, id := range s.bigrams[g] {
			hits[id]++
		}
	}

	// Отсечение: кандидат должен разделять с запросом хотя бы треть биграмм.
	// Без него в нечёткий слой попадает половина справочника.
	minHits := max(len(grams)/3, 1)

	type cand struct {
		id  uint32
		cnt int
	}
	out := make([]cand, 0, limit)
	for id, cnt := range hits {
		if cnt >= minHits {
			out = append(out, cand{id, cnt})
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].cnt > out[b].cnt })
	if len(out) > limit {
		out = out[:limit]
	}

	ids := make([]uint32, len(out))
	for i, c := range out {
		ids[i] = c.id
	}
	return ids
}

/* ── Накопитель результатов ────────────────────────────────────── */

type accumulator struct {
	seen    map[uint32]int // позиция улицы -> индекс в results
	results []Result
	query   Query
	snap    *Snapshot
	qRunes  int // длина нормализованного запроса в рунах
}

func (a *accumulator) add(id uint32, base float64, layer Layer) {
	st := a.snap.Street(id)
	score := base + a.bonus(st)

	if i, ok := a.seen[id]; ok {
		// Улица уже найдена более точным слоем — оставляем лучший балл.
		if score > a.results[i].Score {
			a.results[i].Score = score
			a.results[i].Layer = layer
		}
		return
	}

	a.seen[id] = len(a.results)
	a.results = append(a.results, Result{Street: st, Score: score, Layer: layer})
}

// bonus — доменная часть ранжирования. Именно она отличает нас от
// универсальных сервисов: частота вызовов и история пациента им недоступны.
func (a *accumulator) bonus(st *Street) float64 {
	w := a.query.Weights
	var b float64

	// Логарифм, а не сама частота: разница между 1 и 10 вызовами значима,
	// между 900 и 1000 — нет.
	if st.Calls > 0 {
		b += w.Calls * math.Log1p(float64(st.Calls))
	}

	// Предпочтение региона лечит болезнь «предлагает город вместо улицы»,
	// отмеченную у DaData, Iqdq и Яндекса (docs/01-competitors.md §3.5).
	switch {
	case a.query.Region != "" && st.Region == a.query.Region:
		b += w.Region
	case a.query.Region == "" && st.Region == RegionMoscow:
		b += w.Region
	}

	if a.query.Settlement != "" && st.Settlement == a.query.Settlement {
		b += w.Settlement
	}

	for _, id := range a.query.PatientStreets {
		if id == st.ID {
			b += w.Patient
			break
		}
	}

	// Доля названия, покрытая запросом. Считается по имени БЕЗ типа:
	// диспетчер печатает «саляма адиля», а не «саляма адиля улица».
	if n := len([]rune(st.Name)); n > 0 && a.qRunes > 0 {
		cov := float64(a.qRunes) / float64(n)
		if cov > 1 {
			cov = 1
		}
		b += w.Coverage * cov
		// При коротком запросе покрытие у всех мало и почти не различает.
		// Тогда работает длина названия: из «Ткацкая» и
		// «Верхняя Первомайская» по вводу «т» вероятнее первая.
		if a.qRunes <= 3 {
			b += w.ShortName * (1 - math.Min(float64(n), 30)/30)
		}
	}

	return b
}

// enough — набрано ли достаточно результатов, чтобы не спускаться на
// следующий слой.
func (a *accumulator) enough() bool { return len(a.results) >= a.query.Limit }

// top упорядочивает результаты: сначала по СЛОЮ, затем по баллу.
//
// Слой главнее намеренно. Если сортировать одним общим баллом, доменная
// надбавка перебивает разницу между слоями: на запрос «заводская улица»
// первой выходила «Петрозаводская улица», потому что вызовов на ней в разы
// больше, и надбавка за частоту (6·log(1+N)) перекрывала отрыв точного слоя
// от подстрочного. Точное совпадение обязано быть первым всегда, а частота
// вызовов должна упорядочивать результаты ВНУТРИ слоя, а не между слоями.
func (a *accumulator) top() []Result {
	sort.SliceStable(a.results, func(i, j int) bool {
		if a.results[i].Layer != a.results[j].Layer {
			return a.results[i].Layer < a.results[j].Layer
		}
		if a.results[i].Score != a.results[j].Score {
			return a.results[i].Score > a.results[j].Score
		}
		// Устойчивость выдачи важна: при равных баллах порядок не должен
		// прыгать между запросами, иначе подсказка «дрожит» под курсором.
		return a.results[i].Street.ID < a.results[j].Street.ID
	})

	if len(a.results) > a.query.Limit {
		a.results = a.results[:a.query.Limit]
	}
	return a.results
}
