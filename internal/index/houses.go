package index

import (
	"sort"

	"github.com/zazzool/y-fias/internal/parse"
)

// Дома в снапшоте.
//
// Хранятся отдельно от улиц и подключаются по желанию: их 2.3 млн против
// 85 тыс. улиц. Нужны для двух вещей.
//
// Первая — отличить «дома нет в справочнике ФНС» от «дом не найден». Разница
// существенная: справочник ФНС не содержит части реальных строений (§8.7
// описи), и наивный валидатор на этом ломается, отвергая существующие адреса.
// Поэтому HouseQC различает три состояния, а не два.
//
// Вторая — вывод района по соседним домам, когда координат нет.

// SetHouses подключает дома к снапшоту.
//
// Списки сортируются по ведущему числу: вывод района по соседям (§3.2 описи)
// работает с ближайшими номерами, и без порядка каждый запрос стоил бы прохода
// по всей улице.
func (s *Snapshot) SetHouses(byStreet map[uint32][]HouseRef) {
	if len(byStreet) == 0 {
		return
	}

	s.houses = make(map[uint32][]HouseRef, len(byStreet))
	for id, list := range byStreet {
		refs := make([]HouseRef, 0, len(list))
		for _, h := range list {
			if h.Num == 0 && h.Canonical != "" {
				h.Num = parse.ParseHouse(h.Canonical).NumN
			}
			refs = append(refs, h)
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].Num != refs[j].Num {
				return refs[i].Num < refs[j].Num
			}
			return refs[i].Canonical < refs[j].Canonical
		})
		s.houses[id] = refs
	}
}

// HouseRef — дом справочника.
type HouseRef struct {
	ObjectID  int64  // идентификатор ГАР; нужен синхронизации
	Canonical string // каноническая форма: «7к7»
	Num       int    // ведущее число; ТОЛЬКО для отбора соседей (§8.2)
}

// HouseRefsOf собирает HouseRef из канонических номеров. Удобство для
// тестов и для источников, у которых идентификаторов ГАР нет.
func HouseRefsOf(canonicals ...string) []HouseRef {
	out := make([]HouseRef, 0, len(canonicals))
	for _, c := range canonicals {
		out = append(out, HouseRef{Canonical: c})
	}
	return out
}

// HousesOf возвращает дома улицы, отсортированные по номеру.
func (s *Snapshot) HousesOf(street uint32) []HouseRef { return s.houses[street] }

// HouseCount — сколько домов загружено всего.
func (s *Snapshot) HouseCount() int {
	var n int
	for _, v := range s.houses {
		n += len(v)
	}
	return n
}

// LookupHouse проверяет, есть ли дом на улице.
//
// Сравнение идёт по ПОЛНОЙ канонической форме, а не по ведущему числу:
// «7к7», «7к1» и «7» — три разных дома (§8.2). Ведущее число служит лишь
// для сужения диапазона поиска.
func (s *Snapshot) LookupHouse(street uint32, h parse.House) parse.HouseQC {
	list := s.houses[street]
	if len(list) == 0 {
		// Дома по этой улице не загружены — сказать нечего. Отвечать
		// «не найден» было бы враньём: это разные вещи.
		return parse.HouseUnknown
	}

	canon := h.Canonical()
	if canon == "" {
		return parse.HouseUnknown
	}

	// Диапазон с тем же ведущим числом.
	lo := sort.Search(len(list), func(i int) bool { return list[i].Num >= h.NumN })
	for i := lo; i < len(list) && list[i].Num == h.NumN; i++ {
		if list[i].Canonical == canon {
			return parse.HouseInGar
		}
	}

	// Дома с таким числом на улице есть, а нашего среди них нет. Это НЕ
	// повод отвергнуть адрес: в справочнике ФНС недостаёт части строений, а
	// вызов туда всё равно приедет. Возвращаем «реален, но не в ГАР» —
	// решение принимает человек.
	return parse.HouseNotInGar
}

// NeighborsOf возвращает дома, ближайшие по номеру. Материал для вывода
// района по соседям, когда координат нет.
func (s *Snapshot) NeighborsOf(street uint32, num, limit int) []HouseRef {
	list := s.houses[street]
	if len(list) == 0 || limit <= 0 {
		return nil
	}

	pos := sort.Search(len(list), func(i int) bool { return list[i].Num >= num })

	out := make([]HouseRef, 0, limit)
	lo, hi := pos-1, pos
	for len(out) < limit && (lo >= 0 || hi < len(list)) {
		switch {
		case lo < 0:
			out = append(out, list[hi])
			hi++
		case hi >= len(list):
			out = append(out, list[lo])
			lo--
		case num-list[lo].Num <= list[hi].Num-num:
			out = append(out, list[lo])
			lo--
		default:
			out = append(out, list[hi])
			hi++
		}
	}
	return out
}
