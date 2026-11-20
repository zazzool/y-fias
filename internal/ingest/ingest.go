// Package ingest собирает поисковый снапшот из выгрузки ГАР.
package ingest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
	"github.com/zazzool/y-fias/internal/parse"
)

// Options управляет сборкой.
type Options struct {
	Regions []string
	Log     *slog.Logger

	// WithHouses включает загрузку домов. Дома нужны, чтобы отличать
	// «дома нет в справочнике» от «дом не найден» (HouseQC), но стоят
	// нескольких минут и сотен мегабайт.
	WithHouses bool
}

// Result — что получилось.
type Result struct {
	Streets []index.Street
	// Houses — дома по позиции улицы, с идентификаторами ГАР.
	// Используются для HouseQC, вывода района по соседям и синхронизации.
	Houses  map[uint32][]index.HouseRef
	Version int64
	Elapsed time.Duration
	Fetched int64
}

// levelsWanted — уровни ГАР, которые попадают в подсказки.
//
// Улицы (8) и элементы планировки (7) — то, что вводит диспетчер.
// Населённые пункты (6) и города (5) нужны для области, где адрес без
// посёлка не однозначен. Уровни 9 и 10 (участки и здания) не берём: они
// описывают не «куда ехать», а объект недвижимости.
var levelsWanted = map[uint8]bool{
	gar.LevelCity:      true,
	gar.LevelLocality:  true,
	gar.LevelPlanning:  true,
	gar.LevelStreet:    true,
	gar.LevelInnerCity: true,
}

// levelsContext — уровни, которые нужны только чтобы определить район.
var levelsContext = map[uint8]bool{
	gar.LevelRegion:      true,
	gar.LevelAdmDistrict: true,
	gar.LevelMunDistrict: true,
	gar.LevelSettlement2: true,
	gar.LevelCity:        true,
	gar.LevelLocality:    true,
	gar.LevelInnerCity:   true,
}

// Run собирает снапшот из открытого архива.
func Run(ctx context.Context, a *gar.Archive, version int64, opt Options) (*Result, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	start := time.Now()

	// Шаг 1. Все объекты нужных и контекстных уровней.
	//
	// Читаются вместе, одним проходом: файл AS_ADDR_OBJ невелик (8.8 МБ по
	// двум регионам), а второй проход стоил бы повторного скачивания.
	wanted := map[uint8]bool{}
	for l := range levelsWanted {
		wanted[l] = true
	}
	for l := range levelsContext {
		wanted[l] = true
	}

	objects := map[int64]gar.Object{}
	for _, e := range a.Entries(gar.KindAddrObj, opt.Regions) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := streamEntry(e, func(r io.Reader) (gar.Stats, error) {
			return gar.ParseObjects(r, e.Region, gar.Filter{Levels: wanted},
				func(o gar.Object) { objects[o.ObjectID] = o })
		})
		if err != nil {
			return nil, err
		}
		log.Info("объекты", "регион", e.Region, "взято", st.Kept, "устарело", st.Stale)
	}

	// Шаг 2. Иерархии — только для объектов, которые мы оставили.
	//
	// Фильтр существен: всего связей 14.2 млн, из них к адресным объектам
	// относятся десятки тысяч. Без фильтра снапшот занимал бы вдвое больше
	// памяти ради данных о квартирах и машиноместах.
	//
	// Читаются ОБЕ иерархии, и это выяснилось замером, а не из документации.
	// В административном делении Москвы улица подчинена сразу субъекту:
	// 3 718 улиц из 8 218 имели цепочку «улица -> Москва», без района между
	// ними. Районы (Басманный, Тверской) живут в МУНИЦИПАЛЬНОМ делении.
	// Поэтому район ищется по муниципальной иерархии, а административная
	// остаётся запасной и даёт связь «дом -> улица».
	objFilter := make(map[int64]bool, len(objects))
	for id := range objects {
		objFilter[id] = true
	}

	parentsAdm := make(map[int64]int64, len(objects))
	parentsMun := make(map[int64]int64, len(objects))

	// Связи «дом -> улица» собираются ЗДЕСЬ ЖЕ, попутно.
	//
	// Раньше для них иерархия читалась вторым проходом, а это ещё гигабайт
	// загрузки и несколько минут — притом что нужные связи проходят мимо нас
	// уже в первом проходе. Признак дома простой: родитель есть в списке
	// известных объектов, а сам объект — нет.
	var childOfObject map[int64]int64
	if opt.WithHouses {
		childOfObject = make(map[int64]int64, 1<<21)
	}

	for _, h := range []struct {
		kind string
		dst  map[int64]int64
	}{
		{gar.KindMunHierarchy, parentsMun},
		{gar.KindAdmHierarchy, parentsAdm},
	} {
		for _, e := range a.Entries(h.kind, opt.Regions) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			dst := h.dst
			st, err := streamEntry(e, func(r io.Reader) (gar.Stats, error) {
				// Фильтр не задан: нужны и связи известных объектов, и связи
				// их потомков-домов. Отбор идёт в обработчике.
				return gar.ParseLinks(r, gar.Filter{}, func(l gar.Link) {
					if objFilter[l.ObjectID] {
						dst[l.ObjectID] = l.ParentID
						return
					}
					if childOfObject != nil && objFilter[l.ParentID] {
						childOfObject[l.ObjectID] = l.ParentID
					}
				})
			})
			if err != nil {
				return nil, err
			}
			log.Info("иерархия", "вид", h.kind, "регион", e.Region,
				"просмотрено", st.Seen, "объектов", len(dst), "домов", len(childOfObject))
		}
	}
	// Шаг 3. Улицы с разрешённым районом.
	streets := make([]index.Street, 0, len(objects))
	posByObject := make(map[int64]uint32, len(objects))

	// Диагностика цепочки родителей: без неё непонятно, почему район не
	// разрешился, и остаётся только гадать.
	chainLevels := map[string]int{}
	noDistrict := 0

	for id, o := range objects {
		if !levelsWanted[o.Level] {
			continue
		}
		// Район — из муниципального деления, населённый пункт — откуда
		// нашёлся первым.
		district, settlement := resolveContext(id, objects, parentsMun)
		if district == "" {
			d2, s2 := resolveContext(id, objects, parentsAdm)
			district = d2
			if settlement == "" {
				settlement = s2
			}
		}
		if district == "" {
			noDistrict++
			if len(chainLevels) < 5000 {
				chainLevels[chainOf(id, objects, parentsMun)]++
			}
		}

		posByObject[id] = uint32(len(streets))
		streets = append(streets, index.Street{
			ObjectID:   id,
			ParentID:   parentOf(id, parentsMun, parentsAdm),
			Name:       o.Name,
			Type:       streetType(o),
			Region:     o.Region,
			District:   district,
			Settlement: settlement,
			Source:     parse.SourceGar,
		})
	}
	log.Info("улицы", "всего", len(streets), "без района", noDistrict)
	for typ, n := range topCounts(unknownTypes, 12) {
		log.Info("вид объекта не является типом улицы",
			"вид", typ, "объектов", n,
			"замечание", "если это тип улицы — добавьте в normalize.streetTypes")
	}
	for chain, n := range topChains(chainLevels, 8) {
		log.Info("цепочка без района", "уровни", chain, "объектов", n)
	}

	res := &Result{Streets: streets, Version: version}

	// Шаг 4. Дома — по желанию.
	if opt.WithHouses {
		houses, err := loadHouses(ctx, a, opt, log, childOfObject, posByObject)
		if err != nil {
			return nil, err
		}
		res.Houses = houses
	}

	res.Elapsed = time.Since(start)
	res.Fetched, _ = a.Fetched()
	return res, nil
}

// parentOf — непосредственный родитель объекта. Муниципальная иерархия
// предпочтительнее: именно в ней у Москвы лежат районы.
func parentOf(id int64, mun, adm map[int64]int64) int64 {
	if p, ok := mun[id]; ok && p != 0 {
		return p
	}
	return adm[id]
}

// resolveContext поднимается по иерархии и находит район и населённый пункт.
//
// Район ищется на административном или муниципальном уровне, населённый
// пункт — на уровне города или посёлка. Для Москвы районом оказывается
// внутригородская территория (уровень 17), для области — район области.
func resolveContext(id int64, objects map[int64]gar.Object, parents map[int64]int64) (district, settlement string) {
	const maxDepth = 12 // защита от циклов в данных

	cur := parents[id]
	for range maxDepth {
		if cur == 0 {
			break
		}
		o, ok := objects[cur]
		if !ok {
			break
		}

		switch o.Level {
		case gar.LevelInnerCity, gar.LevelAdmDistrict, gar.LevelMunDistrict:
			if district == "" {
				district = cleanDistrict(o.Name)
			}
		case gar.LevelCity, gar.LevelLocality, gar.LevelSettlement2:
			if settlement == "" {
				settlement = o.Name
			}
		}

		cur = parents[cur]
	}
	return district, settlement
}

// districtPrefixes — служебные слова, которыми ГАР предваряет название района.
//
// В выгрузке район называется «муниципальный округ Басманный», а в нашей базе
// и в справочнике ПНД он записан как «Басманный». Расхождение не косметическое:
// связь «район -> ПНД» устанавливается по ТЕКСТУ (§2.2 описи), и с префиксом
// она не установится ни для одного адреса.
var districtPrefixes = []string{
	"муниципальный округ ",
	"городской округ ",
	"внутригородская территория ",
	"поселение ",
	"городское поселение ",
	"сельское поселение ",
	"район ",
}

func cleanDistrict(name string) string {
	low := strings.ToLower(name)
	for _, p := range districtPrefixes {
		if strings.HasPrefix(low, p) {
			return strings.TrimSpace(name[len(p):])
		}
	}
	// Суффиксная форма: «Раменский район».
	if strings.HasSuffix(low, " район") {
		return strings.TrimSpace(name[:len(name)-len(" район")])
	}
	return name
}

// loadHouses читает дома и раскладывает их по улицам.
func loadHouses(
	ctx context.Context,
	a *gar.Archive,
	opt Options,
	log *slog.Logger,
	houseParent map[int64]int64,
	posByObject map[int64]uint32,
) (map[uint32][]index.HouseRef, error) {
	log.Info("связи домов", "всего", len(houseParent))

	houseFilter := make(map[int64]bool, len(houseParent))
	for id := range houseParent {
		houseFilter[id] = true
	}

	out := make(map[uint32][]index.HouseRef, len(posByObject))
	for _, e := range a.Entries(gar.KindHouses, opt.Regions) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		st, err := streamEntry(e, func(r io.Reader) (gar.Stats, error) {
			return gar.ParseHouses(r, e.Region, gar.Filter{Objects: houseFilter},
				func(h gar.House) {
					pos, ok := posByObject[houseParent[h.ObjectID]]
					if !ok {
						return
					}
					// Пустой номер дома не несёт информации: по нему
					// нельзя ни найти адрес, ни отличить один дом от
					// другого. В ГАР такие записи встречаются.
					canon := canonicalHouse(h)
					if canon == "" {
						return
					}
					out[pos] = append(out[pos], index.HouseRef{
						ObjectID:  h.ObjectID,
						Canonical: canon,
					})
				})
		})
		if err != nil {
			return nil, err
		}
		log.Info("дома", "регион", e.Region, "взято", st.Kept)
	}
	return out, nil
}

// canonicalHouse собирает номер дома в том же виде, что и parse.ParseHouse.
//
// Иначе сравнение «дом из ГАР» с «дом, введённый диспетчером» ничего не
// значило бы: у них должна быть одна каноническая форма.
func canonicalHouse(h gar.House) string {
	var b []byte
	if h.Type == 1 { // владение
		b = append(b, "вл"...)
	}
	b = append(b, h.Num...)

	add := func(num string, typ uint8) {
		if num == "" {
			return
		}
		switch typ {
		case 1:
			b = append(b, "к"...)
		case 2:
			b = append(b, "с"...)
		case 3:
			b = append(b, "соор"...)
		case 4:
			b = append(b, "лит"...)
		default:
			return
		}
		b = append(b, num...)
	}
	add(h.AddNum1, h.AddType1)
	add(h.AddNum2, h.AddType2)

	// Прогон через наш разбор гарантирует, что форма совпадёт с той, что
	// получится из пользовательского ввода: регистр, порядок частей.
	return parse.ParseHouse(string(b)).Canonical()
}

// StreetTypeOf — тип улицы для объекта ГАР. Экспортируется для дельты:
// правило должно быть одним и тем же при полной сборке и при обновлении.
func StreetTypeOf(o gar.Object) string { return streetType(o) }

// CanonicalHouseOf — каноническая форма номера дома из записи ГАР.
// Экспортируется по той же причине.
func CanonicalHouseOf(h gar.House) string { return canonicalHouse(h) }

// streetType возвращает тип улицы для объекта ГАР.
//
// У населённых пунктов ГАР тоже заполняет TYPENAME — «д.», «п.», «г.», «рп.»,
// — но это типы НАСЕЛЁННЫХ ПУНКТОВ, а не улиц, и класть их в то же поле
// нельзя. Иначе «деревня Акулово» получила бы тип «д.», который словарь типов
// улиц не знает, и ключ объекта из справочника не совпал бы с ключом из ввода
// диспетчера. Вид объекта определяется его уровнем, а не написанием типа.
func streetType(o gar.Object) string {
	switch o.Level {
	case gar.LevelSettlement2, gar.LevelCity, gar.LevelLocality:
		// Название посёлка самодостаточно: «Акулово», а не «Акулово д.».
		return ""
	}

	typ := expandType(o.TypeName)
	if typ == "" {
		return ""
	}

	// Тип принимается, только если он ЕСТЬ в словаре типов улиц.
	//
	// В ГАР по области объекты размечены видами, которые типами улиц не
	// являются: «гск» (1 556 объектов), «снт» (808), «зона», «а/я»,
	// «ж/д к-ма». Их названия самодостаточны и уже содержат вид —
	// «СНТ Лесное» с типом «снт» давало ключ «лесное снт снт», где вид
	// удвоился и перестал совпадать с ключом пользовательского ввода.
	//
	// Правило одно и покрывает в том числе виды, которых мы ещё не видели:
	// если это не тип улицы, значит это вид места, а название места стоит
	// само за себя. Неизвестные виды ингест перечисляет в логе, чтобы
	// настоящий тип улицы можно было заметить и добавить в словарь.
	if !normalize.IsStreetType(normalize.SearchNorm(typ)) {
		unknownTypes[o.TypeName]++
		return ""
	}
	return typ
}

// unknownTypes копит виды объектов, не опознанные как типы улиц.
var unknownTypes = map[string]int{}

// expandType разворачивает сокращение типа улицы из ГАР в канонический вид.
// В ГАР это «ул», «пер», «ш»; в подсказке нужно «улица», «переулок», «шоссе».
func expandType(short string) string {
	if short == "" {
		return ""
	}
	// SplitStreetType умеет и полные названия, и сокращения; отдаём ему
	// строку вида «X ул», чтобы получить канонический тип.
	_, typ := splitType("объект " + short)
	if typ != "" {
		return typ
	}
	return short
}

// chainOf возвращает цепочку уровней от объекта вверх: «8 -> 3 -> 1».
func chainOf(id int64, objects map[int64]gar.Object, parents map[int64]int64) string {
	var b []string
	if o, ok := objects[id]; ok {
		b = append(b, fmt.Sprint(o.Level))
	}
	cur := parents[id]
	for range 8 {
		if cur == 0 {
			b = append(b, "нет")
			break
		}
		o, ok := objects[cur]
		if !ok {
			b = append(b, "?"+fmt.Sprint(cur))
			break
		}
		b = append(b, fmt.Sprint(o.Level))
		cur = parents[cur]
	}
	return strings.Join(b, " -> ")
}

// topCounts — n самых частых ключей карты.
func topCounts(m map[string]int, n int) map[string]int {
	return topChains(m, n)
}

func topChains(m map[string]int, n int) map[string]int {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })
	out := map[string]int{}
	for i, e := range all {
		if i >= n {
			break
		}
		out[e.k] = e.v
	}
	return out
}

func streamEntry(e gar.Entry, fn func(io.Reader) (gar.Stats, error)) (gar.Stats, error) {
	rc, err := e.Open()
	if err != nil {
		return gar.Stats{}, fmt.Errorf("открытие %s: %w", e.Kind, err)
	}
	defer func() { _ = rc.Close() }()
	return fn(rc)
}
