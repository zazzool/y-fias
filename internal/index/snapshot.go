// Package index — поисковый индекс адресов, целиком в оперативной памяти.
//
// Замер этапа 0 показал: справочник Москвы и области занимает 509 МБ в худшем
// представлении (docs/04-stage0-findings.md §1). Значит на горячем пути не
// нужны ни сеть, ни диск, ни SQL — и подсказки продолжают работать даже когда
// недоступен PostgreSQL. В прежней архитектуре это было невозможно.
package index

import (
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/zazzool/y-fias/internal/normalize"
	"github.com/zazzool/y-fias/internal/parse"
)

// Street — адресный объект: улица, посёлок, элемент планировки.
type Street struct {
	ID   uint32
	Name string // «Тверская» — без типа
	Type string // «улица» — канонический тип

	Key  string // normalize.StreetKey: тождество
	Norm string // normalize.SearchNorm: подстрочный и нечёткий поиск

	Region     string // «77», «50»
	District   string
	Settlement string // населённый пункт, СНТ — различает одноимённые улицы

	// Calls — сколько вызовов приходится на адреса этой улицы.
	//
	// Это наш дифференциатор. Универсальные адресные сервисы такого сигнала
	// не имеют в принципе: у них нет 92 000 вызовов конкретной подстанции.
	Calls uint32

	Source parse.Source
}

// Full — «человеческое» название с типом.
func (s *Street) Full() string {
	if s.Type == "" {
		return s.Name
	}
	return s.Name + " " + s.Type
}

// Label — подпись для выпадающего списка. Вне Москвы дописывается контекст,
// иначе одноимённые СНТ разных районов неразличимы (§3.5 описи).
func (s *Street) Label() string {
	base := s.Full()
	switch {
	case s.Settlement != "" && s.District != "":
		return base + " (" + s.Settlement + ", " + s.District + ")"
	case s.Settlement != "":
		return base + " (" + s.Settlement + ")"
	case s.Region != "" && s.Region != RegionMoscow && s.District != "":
		return base + " (" + s.District + ")"
	}
	return base
}

const RegionMoscow = "77"

// Snapshot — неизменяемый срез справочника.
//
// Собирается целиком и подменяется одним atomic.Pointer.Store, поэтому
// читатели работают БЕЗ блокировок, а перестройка не влияет на задержки.
// Наблюдать полусобранное состояние невозможно; откат — это Store предыдущего.
type Snapshot struct {
	Version int64
	BuiltAt time.Time

	streets []Street

	// Слой точного совпадения: ключ улицы -> позиции в streets.
	byKey map[string][]uint32

	// Слой префикса: нормализованные имена, отсортированные для бинарного
	// поиска границ диапазона. Хранится параллельными массивами.
	prefixKeys []string
	prefixIDs  []uint32
	// prefixInner отмечает ключи, полученные из ВНУТРЕННЕГО слова названия
	// («адиля» от «Саляма Адиля»), а не от его начала. Такие совпадения
	// ранжируются ниже: ввод чаще начинают с первого слова.
	prefixInner []bool

	// Слой нечёткого поиска: биграмма -> позиции.
	bigrams map[uint32][]uint32
}

// Len — сколько объектов в снапшоте.
func (s *Snapshot) Len() int { return len(s.streets) }

// Street возвращает объект по позиции.
func (s *Snapshot) Street(i uint32) *Street { return &s.streets[i] }

// Build собирает снапшот из набора улиц.
//
// Порядок входа не важен: индексы строятся заново. Функция намеренно
// принимает готовый срез, а не источник данных, — сборка снапшота и добыча
// данных разделены, чтобы одно можно было тестировать без другого.
func Build(streets []Street, version int64) *Snapshot {
	s := &Snapshot{
		Version: version,
		BuiltAt: time.Now(),
		streets: streets,
		byKey:   make(map[string][]uint32, len(streets)),
		bigrams: make(map[uint32][]uint32, len(streets)*4),
	}

	for i := range s.streets {
		st := &s.streets[i]
		st.ID = uint32(i)

		// Ключи вычисляются здесь, а не доверяются источнику: иначе снова
		// появятся расходящиеся копии правил нормализации (§8.5).
		if st.Key == "" {
			st.Key = normalize.StreetKey(st.Full())
		}
		if st.Norm == "" {
			st.Norm = normalize.SearchNorm(st.Full())
		}

		s.byKey[st.Key] = append(s.byKey[st.Key], uint32(i))
	}

	s.buildPrefix()
	s.buildBigrams()
	return s
}

func (s *Snapshot) buildPrefix() {
	type entry struct {
		key   string
		id    uint32
		inner bool
	}

	// Улица индексируется тремя способами:
	//   1. полное нормализованное название — «саляма адиля улица»;
	//   2. имя без типа — диспетчер печатает «саляма», а не «саляма улица»;
	//   3. КАЖДОЕ СЛОВО имени — «адиля» должно находить «Саляма Адиля».
	//
	// Третий пункт существенен для стоимости: без него запросы вроде «адиля»
	// проваливались бы в подстрочный слой с линейным проходом по всему
	// справочнику, а так они обслуживаются бинарным поиском.
	entries := make([]entry, 0, len(s.streets)*4)
	seen := make(map[string]bool, 8)

	for i := range s.streets {
		st := &s.streets[i]
		clear(seen)

		add := func(k string, inner bool) {
			if k == "" || seen[k] {
				return
			}
			seen[k] = true
			entries = append(entries, entry{k, uint32(i), inner})
		}

		add(st.Norm, false)
		nameNorm := normalize.SearchNorm(st.Name)
		add(nameNorm, false)

		words := strings.Fields(nameNorm)
		for wi, w := range words {
			// Однобуквенные слова как ключ бесполезны: они дают сотни
			// совпадений и только раздувают индекс.
			if len([]rune(w)) > 1 {
				add(w, wi > 0)
			}
		}
	}

	sort.Slice(entries, func(a, b int) bool {
		if entries[a].key != entries[b].key {
			return entries[a].key < entries[b].key
		}
		return entries[a].id < entries[b].id
	})

	s.prefixKeys = make([]string, len(entries))
	s.prefixIDs = make([]uint32, len(entries))
	s.prefixInner = make([]bool, len(entries))
	for i, e := range entries {
		s.prefixKeys[i] = e.key
		s.prefixIDs[i] = e.id
		s.prefixInner[i] = e.inner
	}
}

func (s *Snapshot) buildBigrams() {
	seen := make(map[uint32]bool, 32)
	for i := range s.streets {
		clear(seen)
		for _, bg := range bigramsOf(s.streets[i].Norm) {
			if seen[bg] {
				continue
			}
			seen[bg] = true
			s.bigrams[bg] = append(s.bigrams[bg], uint32(i))
		}
	}
}

/* ── Держатель снапшота ────────────────────────────────────────── */

// Holder хранит текущий снапшот и позволяет подменить его атомарно.
//
// Чтения идут через Load() без блокировок. Старый снапшот собирает сборщик
// мусора, когда его отпустят последние выполняющиеся запросы, поэтому
// подмена безопасна под нагрузкой.
type Holder struct {
	cur atomic.Pointer[Snapshot]
}

func NewHolder(s *Snapshot) *Holder {
	h := &Holder{}
	h.cur.Store(s)
	return h
}

// Load — текущий снапшот. Дешёвая операция, вызывать на каждый запрос.
func (h *Holder) Load() *Snapshot { return h.cur.Load() }

// Store подменяет снапшот.
func (h *Holder) Store(s *Snapshot) { h.cur.Store(s) }

// Age — возраст текущего снапшота. Метрика snapshot_age_seconds (§11.6).
func (h *Holder) Age() time.Duration {
	s := h.cur.Load()
	if s == nil {
		return 0
	}
	return time.Since(s.BuiltAt)
}
