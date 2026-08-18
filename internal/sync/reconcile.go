package sync

import (
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
)

// Сверка справочника с нашими адресами.
//
// Наивная синхронизация «обновить справочник» была бы неполной и опасной.
// ГАР для нас справочник, а не начальство над записями: в addresses лежат
// адреса, куда уже выезжали бригады, и они не могут менять написание оттого,
// что ФНС что-то переименовала.
//
// Инвариант 1 действует и здесь: сервис не переписывает наши данные, а
// складывает расхождения в очередь, которую разбирает человек.

// ChangeKind — вид расхождения.
type ChangeKind uint8

const (
	// ChangeRenamed — улица переименована в ГАР.
	ChangeRenamed ChangeKind = iota
	// ChangeRetired — объект исчез из справочника или упразднён.
	ChangeRetired
	// ChangeDistrict — сменился район объекта.
	ChangeDistrict
)

func (k ChangeKind) String() string {
	switch k {
	case ChangeRenamed:
		return "переименование"
	case ChangeRetired:
		return "упразднение"
	case ChangeDistrict:
		return "смена района"
	}
	return "?"
}

// Change — расхождение, требующее решения человека.
type Change struct {
	Kind ChangeKind

	// AddressIDs — наши адреса, которых касается изменение.
	AddressIDs []string

	OldValue string // как записано у нас
	NewValue string // как стало в ГАР
	Note     string // пояснение для того, кто будет разбирать
}

// OurAddress — минимум сведений о нашем адресе для сверки.
type OurAddress struct {
	ID       string
	Street   string
	House    string
	District string
}

// Report — итог сверки.
type Report struct {
	Total      int // наших адресов всего
	MatchedGar int // найдено в справочнике

	Changes []Change
}

// MatchRate — доля наших адресов, найденных в справочнике.
//
// Метрика §11.6 архитектуры. Она НЕ будет равна единице: справочник ФНС не
// содержит части реальных строений (§8.7 описи). Значение имеет не сам
// уровень, а его РЕЗКОЕ падение после синхронизации — это признак того, что
// дельта применилась неверно, и повод откатиться.
func (r Report) MatchRate() float64 {
	if r.Total == 0 {
		return 0
	}
	return float64(r.MatchedGar) / float64(r.Total)
}

// Reconcile сверяет наши адреса с новым состоянием справочника.
//
// Функция НИЧЕГО не меняет: она только описывает расхождения.
func Reconcile(ours []OurAddress, before, after *index.Snapshot) Report {
	rep := Report{Total: len(ours)}

	// Ключ улицы -> объекты справочника.
	afterByKey := indexByKey(after)
	beforeByKey := indexByKey(before)

	// Наши адреса, сгруппированные по ключу улицы: расхождение касается
	// сразу всех адресов улицы, и перечислять их по одному незачем.
	oursByKey := map[string][]OurAddress{}
	for _, a := range ours {
		k := normalize.StreetKey(a.Street)
		if k == "" {
			continue
		}
		oursByKey[k] = append(oursByKey[k], a)
		if len(afterByKey[k]) > 0 {
			rep.MatchedGar++
		}
	}

	for key, group := range oursByKey {
		nowStreets := afterByKey[key]

		if len(nowStreets) == 0 {
			// Улицы больше нет в справочнике. Адрес НЕ удаляется: туда
			// выезжала бригада, это факт истории. Но если раньше она была,
			// человек должен об этом узнать.
			if len(beforeByKey[key]) > 0 {
				rep.Changes = append(rep.Changes, Change{
					Kind:       ChangeRetired,
					AddressIDs: idsOf(group),
					OldValue:   group[0].Street,
					Note: "улица исчезла из справочника ГАР; адрес сохранён, " +
						"проверьте, не переименована ли она",
				})
			}
			continue
		}

		// Смена района. Записанный район НЕ меняется — подсказка обновится
		// сама, а решение принимает человек.
		garDistrict := nowStreets[0].District
		for _, a := range group {
			if a.District == "" || garDistrict == "" {
				continue
			}
			if !sameDistrict(a.District, garDistrict) {
				rep.Changes = append(rep.Changes, Change{
					Kind:       ChangeDistrict,
					AddressIDs: []string{a.ID},
					OldValue:   a.District,
					NewValue:   garDistrict,
					Note: "район в ГАР отличается от записанного; " +
						"от района зависит ПНД",
				})
			}
		}
	}

	// Переименования: объект справочника сменил название, но остался тем же.
	// Ловится по совпадению района и населённого пункта при разном ключе —
	// точное сопоставление требует идентификаторов ГАР, которые появятся,
	// когда снапшот начнёт их хранить.
	rep.Changes = append(rep.Changes, detectRenames(oursByKey, beforeByKey, afterByKey)...)

	return rep
}

func indexByKey(s *index.Snapshot) map[string][]*index.Street {
	out := map[string][]*index.Street{}
	if s == nil {
		return out
	}
	for i := range s.Len() {
		st := s.Street(uint32(i))
		out[st.Key] = append(out[st.Key], st)
	}
	return out
}

// detectRenames ищет улицы, пропавшие из справочника, для которых в том же
// районе появилась новая с похожим названием.
func detectRenames(
	oursByKey map[string][]OurAddress,
	before, after map[string][]*index.Street,
) []Change {
	var out []Change

	for key, group := range oursByKey {
		if len(after[key]) > 0 || len(before[key]) == 0 {
			continue
		}
		old := before[key][0]

		// Кандидат: объект того же района, появившийся в новой версии.
		for newKey, streets := range after {
			if len(before[newKey]) > 0 {
				continue // не новый
			}
			st := streets[0]
			if !sameDistrict(st.District, old.District) || st.Settlement != old.Settlement {
				continue
			}
			out = append(out, Change{
				Kind:       ChangeRenamed,
				AddressIDs: idsOf(group),
				OldValue:   old.Full(),
				NewValue:   st.Full(),
				Note: "вероятное переименование: прежняя улица исчезла, " +
					"в том же районе появилась новая. Наш адрес НЕ изменён",
			})
			break
		}
	}
	return out
}

func sameDistrict(a, b string) bool {
	return normalize.SearchNorm(a) == normalize.SearchNorm(b)
}

func idsOf(group []OurAddress) []string {
	out := make([]string, 0, len(group))
	for _, a := range group {
		out = append(out, a.ID)
	}
	return out
}
