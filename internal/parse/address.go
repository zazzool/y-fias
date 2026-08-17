package parse

import (
	"strings"

	"github.com/zazzool/y-fias/internal/normalize"
)

// Address — разобранная адресная строка.
//
// Каждое поле сопровождается спаном в исходной строке (Spans), поэтому
// интерфейс может подсветить, что именно понято, а что осталось хвостом.
// Приём заимствован у Ahunter (docs/01-competitors.md §2.2): сервисы,
// которые молча выбрасывают нераспознанный хвост, лишают человека
// возможности заметить ошибку.
type Address struct {
	Raw string

	Region     string // код региона: «77», «50»
	District   string // район
	Settlement string // населённый пункт, СНТ, посёлок
	Street     string // название улицы БЕЗ типа
	StreetType string // канонический тип: «улица», «шоссе»
	House      House

	Tail    string // нераспознанный остаток
	Quality Quality
	Spans   []Span
}

// StreetFull собирает «человеческое» название улицы с типом.
func (a Address) StreetFull() string {
	switch {
	case a.Street == "":
		return ""
	case a.StreetType == "":
		return a.Street
	default:
		return a.Street + " " + a.StreetType
	}
}

// Key — ключ улицы для установления тождества.
func (a Address) Key() string { return normalize.StreetKey(a.StreetFull()) }

// IsEmpty сообщает, что разобрать не удалось ничего.
func (a Address) IsEmpty() bool {
	return a.Street == "" && a.Settlement == "" && a.House.IsEmpty()
}

/* ── Словари сегментов ─────────────────────────────────────────── */

// regionMarkers — приведение названия региона к коду. Пока нужны только два:
// 10 649 адресов из 10 694 — Москва, ещё 42 — область (§2.3 описи).
var regionMarkers = []struct {
	words []string
	code  string
}{
	{[]string{"москва", "г москва", "город москва"}, "77"},
	{[]string{"московская область", "московская обл", "мо", "подмосковье"}, "50"},
}

// settlementTypes — типы населённых пунктов. Это НЕ типы улиц: смешивать их
// нельзя, иначе «посёлок Лесной» и «Лесная улица» дадут один ключ.
var settlementTypes = map[string]string{
	"г": "город", "город": "город", "гор": "город",
	"п": "посёлок", "пос": "посёлок", "поселок": "посёлок", "посёлок": "посёлок",
	"рп": "посёлок", "пгт": "посёлок",
	"д": "деревня", "дер": "деревня", "деревня": "деревня",
	"с": "село", "село": "село", "c": "село",
	"снт": "СНТ", "днп": "ДНП", "тсн": "ТСН", "тлпх": "ТЛПХ",
	"мкр": "микрорайон", "микрорайон": "микрорайон",
	"жк": "ЖК", "кп": "КП",
}

// districtMarkers — признаки того, что сегмент описывает район.
var districtMarkers = []string{"район", "р-он", "р-н", "рн"}

/* ── Разбор ────────────────────────────────────────────────────── */

// ParseAddress разбирает свободную адресную строку.
//
// Функция не изменяет ввод: Raw сохраняется, всё непонятое попадает в Tail,
// а не выбрасывается (инвариант 1).
func ParseAddress(raw string) Address {
	a := Address{Raw: raw}

	s := strings.TrimSpace(raw)
	if s == "" {
		a.Quality.Parse = ParseGarbage
		return a
	}

	// Содержимое скобок — контекст населённого пункта:
	// «Вишнёвая улица (СНТ Лесная поляна, Раменский р-н)».
	s, inParens := cutParens(s)

	segments := splitSegments(s)
	if inParens != "" {
		segments = append(segments, splitSegments(inParens)...)
	}

	var (
		streetSeg  string
		houseSeg   string
		tailParts  []string
		streetSeen bool
	)

	for _, seg := range segments {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}

		switch {
		case a.Region == "" && matchRegion(seg) != "":
			a.Region = matchRegion(seg)

		case isDistrictSegment(seg):
			if a.District == "" {
				a.District = stripDistrictMarker(seg)
			}

		case isSettlementSegment(seg):
			if a.Settlement == "" {
				a.Settlement = seg
			} else {
				tailParts = append(tailParts, seg)
			}

		case isHouseSegment(seg):
			if houseSeg == "" {
				houseSeg = stripHouseMarker(seg)
			} else {
				tailParts = append(tailParts, seg)
			}

		default:
			// Улицей считается первый сегмент, не опознанный иначе.
			if !streetSeen {
				streetSeg = seg
				streetSeen = true
			} else {
				tailParts = append(tailParts, seg)
			}
		}
	}

	// Если сегмент улицы не выделен запятыми, дом мог остаться внутри него:
	// «Тверская улица 7к1», «Тверская 7 к 1».
	if houseSeg == "" && streetSeg != "" {
		streetSeg, houseSeg = splitTrailingHouse(streetSeg)
	}

	if streetSeg != "" {
		a.Street, a.StreetType = normalize.SplitStreetType(streetSeg)
	}
	if houseSeg != "" {
		a.House = ParseHouse(houseSeg)
	}

	a.Tail = strings.Join(tailParts, ", ")
	a.Quality.Parse = gradeParse(a)
	a.Quality.Source = SourceUser
	a.Spans = buildSpans(raw, streetSeg, houseSeg, a.Settlement)
	return a
}

func gradeParse(a Address) ParseQC {
	switch {
	case a.IsEmpty():
		return ParseGarbage
	case a.Tail != "" || a.House.Flags.Has(FlagUnparsed):
		return ParseWithTail
	default:
		return ParseOK
	}
}

// cutParens вырезает содержимое скобок и возвращает остаток и содержимое.
func cutParens(s string) (rest, inside string) {
	open := strings.IndexByte(s, '(')
	if open < 0 {
		return s, ""
	}
	closeIdx := strings.IndexByte(s[open:], ')')
	if closeIdx < 0 {
		return s, ""
	}
	closeIdx += open

	inside = strings.TrimSpace(s[open+1 : closeIdx])
	rest = strings.TrimSpace(s[:open] + " " + s[closeIdx+1:])
	return rest, inside
}

// splitSegments делит строку по запятым. Точка с запятой встречается в
// импортированных данных и трактуется так же.
func splitSegments(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';'
	})
}

func matchRegion(seg string) string {
	n := normalize.SearchNorm(seg)
	for _, r := range regionMarkers {
		for _, w := range r.words {
			if n == w {
				return r.code
			}
		}
	}
	return ""
}

func isDistrictSegment(seg string) bool {
	n := normalize.SearchNorm(seg)
	for _, m := range districtMarkers {
		mn := normalize.SearchNorm(m)
		if mn == "" {
			continue
		}
		if strings.HasSuffix(n, " "+mn) || strings.HasPrefix(n, mn+" ") {
			return true
		}
	}
	return false
}

func stripDistrictMarker(seg string) string {
	n := normalize.SearchNorm(seg)
	for _, m := range districtMarkers {
		mn := normalize.SearchNorm(m)
		n = strings.TrimSuffix(n, " "+mn)
		n = strings.TrimPrefix(n, mn+" ")
	}
	return strings.TrimSpace(n)
}

func isSettlementSegment(seg string) bool {
	fields := strings.Fields(normalize.SearchNorm(seg))
	if len(fields) < 2 {
		return false
	}
	// Тип населённого пункта — первым словом. Требование двух слов
	// защищает от того, чтобы одинокое «с» или «д» съело номер дома.
	_, ok := settlementTypes[fields[0]]
	if !ok {
		return false
	}
	// «д 7» — это дом, а не деревня.
	return !startsWithDigit(fields[1])
}

// isHouseSegment распознаёт сегмент, целиком описывающий дом.
func isHouseSegment(seg string) bool {
	fields := strings.Fields(normalize.SearchNorm(seg))
	if len(fields) == 0 {
		return false
	}
	if _, ok := houseSegMarkers[fields[0]]; ok {
		return true
	}
	// Сегмент из одних «домовых» токенов: «7к1», «7 к 1».
	for _, f := range fields {
		if !isHouseToken(f) {
			return false
		}
	}
	// Одиночный токен годится, только если он сам похож на номер: «7к1» или
	// «вл15». Иначе любое односложное название улицы стало бы домом.
	return startsWithDigit(fields[0]) || isMarkerToken(fields[0]) || len(fields) > 1
}

var houseSegMarkers = map[string]bool{
	"д": true, "дом": true, "вл": true, "влад": true, "владение": true,
	"двл": true, "домовладение": true,
}

func stripHouseMarker(seg string) string {
	fields := strings.Fields(seg)
	if len(fields) > 1 {
		if houseSegMarkers[normalize.SearchNorm(fields[0])] {
			// «д» отбрасываем, «вл» оставляем: владение — часть тождества.
			if normalize.SearchNorm(fields[0]) == "д" || normalize.SearchNorm(fields[0]) == "дом" {
				return strings.Join(fields[1:], " ")
			}
		}
	}
	return seg
}

// houseMarkerWords — обозначения, из которых может состоять номер дома.
// Порядок от длинных к коротким: иначе «стр» съел бы начало «строение».
var houseMarkerWords = []string{
	"домовладение", "владение", "сооружение", "строение", "корпус",
	"литера", "строен", "влад", "корп", "соор", "стр", "лит", "дом",
	"кор", "двл", "вл", "д", "к", "с",
}

// isHouseToken — токен, который может входить в номер дома.
func isHouseToken(t string) bool {
	if t == "" {
		return false
	}
	if startsWithDigit(t) {
		return true
	}
	for _, m := range houseMarkerWords {
		if t == m {
			return true
		}
		// Маркер слитно с числом: «вл15», «д7», «с26». Такое написание
		// в проде обычное: «Гвардейская ул., с26, село Красный Путь».
		if rest, ok := strings.CutPrefix(t, m); ok && startsWithDigit(rest) {
			return true
		}
	}
	// Одиночная буква — литера.
	return len([]rune(t)) == 1
}

// splitTrailingHouse отделяет номер дома, приписанный к названию улицы.
//
// Разбор идёт С КОНЦА и забирает «домовые» токены, пока не встретится слово,
// домовым не являющееся. Требуется, чтобы хотя бы один токен остался улице:
// иначе «8 Марта» превратилось бы в дом 8 без улицы.
func splitTrailingHouse(seg string) (street, house string) {
	fields := strings.Fields(seg)
	if len(fields) < 2 {
		return seg, ""
	}

	cut := len(fields)
	for i := len(fields) - 1; i >= 1; i-- {
		f := normalize.SearchNorm(fields[i])
		if normalize.IsStreetType(f) {
			break
		}
		if !isHouseToken(f) {
			break
		}
		cut = i
	}

	if cut == len(fields) {
		return seg, ""
	}
	// Дом обязан начинаться с цифры или маркера: иначе одиночная буква в
	// конце названия («улица 8 Марта а») утащила бы за собой смысл.
	first := normalize.SearchNorm(fields[cut])
	if !startsWithDigit(first) && !isMarkerToken(first) {
		return seg, ""
	}

	return strings.Join(fields[:cut], " "), strings.Join(fields[cut:], " ")
}

// isMarkerToken — токен целиком или слитно с числом является обозначением
// части дома: «вл», «вл15», «д», «д7».
func isMarkerToken(t string) bool {
	for _, m := range houseMarkerWords {
		if t == m {
			return true
		}
		if rest, ok := strings.CutPrefix(t, m); ok && startsWithDigit(rest) {
			return true
		}
	}
	return false
}

func startsWithDigit(s string) bool {
	for _, r := range s {
		return isDigit(r)
	}
	return false
}

// buildSpans привязывает разобранные поля к фрагментам исходной строки.
func buildSpans(raw, street, house, settlement string) []Span {
	spans := make([]Span, 0, 3)
	add := func(field, value string) {
		if value == "" {
			return
		}
		if i := strings.Index(raw, value); i >= 0 {
			spans = append(spans, Span{Field: field, From: i, To: i + len(value)})
		}
	}
	add("street", street)
	add("house", house)
	add("settlement", settlement)
	return spans
}
