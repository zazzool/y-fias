// Package parse разбирает пользовательский ввод адреса.
//
// Ядро пакета — грамматика номера дома. Приоритет обоснован данными: из 10 694
// адресов прода 6 036 содержат кириллицу в номере дома, а уникальных написаний
// 2 033. Модель «дом = целое число» неверна не только по нашим данным, но и по
// государственному классификатору: ГАР хранит номер как HOUSENUM + HOUSETYPE
// плюс два слота ADDNUM/ADDTYPE.
package parse

import (
	"strings"
	"unicode"
)

// HouseKind — тип основного номера. Значения совпадают с AS_HOUSE_TYPES ГАР.
type HouseKind uint8

const (
	KindUnknown   HouseKind = 0
	KindOwnership HouseKind = 1 // владение
	KindHouse     HouseKind = 2 // дом
	KindEstate    HouseKind = 3 // домовладение
	KindGarage    HouseKind = 4 // гараж
)

// AddKind — тип дополнительной части. Совпадает с AS_ADDHOUSE_TYPES ГАР.
type AddKind uint8

const (
	AddNone      AddKind = 0
	AddCorpus    AddKind = 1 // корпус
	AddBuilding  AddKind = 2 // строение
	AddStructure AddKind = 3 // сооружение
	AddLiter     AddKind = 4 // литера
)

// prefix — обозначение части в канонической форме.
func (k AddKind) prefix() string {
	switch k {
	case AddCorpus:
		return "к"
	case AddBuilding:
		return "с"
	case AddStructure:
		return "соор"
	case AddLiter:
		return "лит"
	}
	return ""
}

// Flags — замечания к разбору. Ни один из них не даёт права изменить ввод.
type Flags uint16

const (
	// FlagHomoglyph — во вводе была латиница или цифра-двойник на месте
	// кириллицы: «7r7» вместо «7к7», «З2к1» вместо «32к1».
	FlagHomoglyph Flags = 1 << iota
	// FlagWordNumber — номер записан прописью: «ДВА», «ШЕсТЬк2».
	// Это след аварии §8.2: так диспетчеры обходили подмену номера дома.
	FlagWordNumber
	// FlagUnparsed — остался неразобранный хвост.
	FlagUnparsed
	// FlagCorner — угловой дом: «1/18».
	FlagCorner
	// FlagEmpty — пустой или бессмысленный ввод.
	FlagEmpty
)

func (f Flags) Has(x Flags) bool { return f&x != 0 }

// Span — привязка разобранного поля к фрагменту исходной строки.
// Позволяет интерфейсу подсветить, что именно он понял, а что нет.
type Span struct {
	Field string
	From  int // байтовые смещения в Raw
	To    int
}

// HousePart — дополнительная часть номера: корпус, строение, сооружение, литера.
type HousePart struct {
	Kind AddKind
	Num  string
}

// House — разобранный номер дома.
//
// Raw хранится неизменным и показывается пользователю (инвариант 1).
// Canonical() служит ТОЛЬКО для установления тождества.
type House struct {
	Raw string

	Kind  HouseKind
	Num   string // «7», «1/18» для углового дома
	NumN  int    // ведущее число; ТОЛЬКО предфильтр кандидатов (§8.2)
	Parts []HousePart

	Flags Flags
	Spans []Span
}

// IsEmpty сообщает, что разбирать было нечего.
func (h House) IsEmpty() bool { return h.Num == "" && len(h.Parts) == 0 }

// Corpus, Building, Liter — доступ к частям по типу.
func (h House) Corpus() string   { return h.part(AddCorpus) }
func (h House) Building() string { return h.part(AddBuilding) }
func (h House) Liter() string    { return h.part(AddLiter) }

func (h House) part(k AddKind) string {
	for _, p := range h.Parts {
		if p.Kind == k {
			return p.Num
		}
	}
	return ""
}

// Canonical — форма для сравнения на тождество.
//
// Правило §8.2, добытое жалобой: «7 к 7» и «7к7» — один дом, но «7к7», «7к1»
// и «7» — три РАЗНЫХ дома. Ведущее число (NumN) здесь не участвует: оно годится
// только для предварительного отбора кандидатов. Подмена этого правила стоила
// диспетчеру чужого адреса в карте 2610837.
//
// Части выводятся в фиксированном порядке (корпус, строение, сооружение,
// литера) независимо от порядка ввода, поэтому «10с4к2» и «10к2с4» тождественны.
func (h House) Canonical() string {
	if h.IsEmpty() {
		return ""
	}

	var b strings.Builder
	b.Grow(len(h.Raw) + 4)

	// Признак владения выводится, только если есть сам номер. Иначе «вл»
	// сливается с обозначением следующей части: «вл с0» давало «влс0», где
	// «вл» уже не отделить от «с», и повторный разбор возвращал пустоту.
	// Владение без номера бессмысленно, терять тут нечего. Нашёл фаззинг.
	if h.Num != "" {
		switch h.Kind {
		case KindOwnership:
			b.WriteString("вл")
		case KindEstate:
			b.WriteString("двл")
		}
	}
	b.WriteString(h.Num)

	for _, k := range []AddKind{AddCorpus, AddBuilding, AddStructure, AddLiter} {
		for _, p := range h.Parts {
			if p.Kind == k {
				b.WriteString(k.prefix())
				b.WriteString(p.Num)
			}
		}
	}
	return b.String()
}

// String — «человеческое» представление. Всегда исходный ввод.
func (h House) String() string { return h.Raw }

/* ── Разбор ────────────────────────────────────────────────────── */

// markers — обозначения частей. Проверяются от длинных к коротким, поэтому
// «строение» не будет разобрано как «с» + «троение».
var markers = []struct {
	word string
	kind AddKind
}{
	{"корпус", AddCorpus},
	{"корп", AddCorpus},
	{"кор", AddCorpus},
	{"строение", AddBuilding},
	{"строен", AddBuilding},
	{"стр", AddBuilding},
	{"сооружение", AddStructure},
	{"соор", AddStructure},
	{"литера", AddLiter},
	{"лит", AddLiter},
	{"к", AddCorpus},
	{"с", AddBuilding},
}

// kindMarkers — обозначения типа основного номера, в начале строки.
var kindMarkers = []struct {
	word string
	kind HouseKind
}{
	{"домовладение", KindEstate},
	{"владение", KindOwnership},
	{"влад", KindOwnership},
	{"вл", KindOwnership},
	{"двлд", KindEstate},
	{"двл", KindEstate},
	{"дом", KindHouse},
	{"д", KindHouse},
}

// wordNumbers — номера прописью. Встречаются в проде как обход дефекта §8.2:
// у слова нет ведущего числа, поэтому house_num() возвращал NULL и адрес не
// приклеивался к чужому. Разбираем, но помечаем флагом.
var wordNumbers = map[string]string{
	"один": "1", "два": "2", "три": "3", "четыре": "4", "пять": "5",
	"шесть": "6", "семь": "7", "восемь": "8", "девять": "9", "десять": "10",
}

// ParseHouse разбирает номер дома.
//
// Функция НИКОГДА не изменяет ввод: результат несёт Raw как есть, а все
// замечания выражены флагами. Решение о применении исправления принимает
// человек (инвариант 1).
func ParseHouse(raw string) House {
	h := House{Raw: raw}

	s, homo := foldHomoglyphs(strings.TrimSpace(raw))
	if homo {
		h.Flags |= FlagHomoglyph
	}
	if s == "" {
		h.Flags |= FlagEmpty
		return h
	}

	// Регистр уже приведён в foldHomoglyphs.
	p := &parser{src: []rune(s)}

	// Тип основного номера, если указан явно.
	if k, ok := p.kindMarker(); ok {
		h.Kind = k
	}
	p.skipSeparators()

	// Основной номер.
	num, isWord := p.baseNumber()
	if isWord {
		h.Flags |= FlagWordNumber
	}
	h.Num = num
	if strings.ContainsAny(num, "/") {
		h.Flags |= FlagCorner
	}
	h.NumN = leadingInt(num)

	// Дополнительные части.
	for {
		p.skipSeparators()
		kind, ok := p.marker()
		if !ok {
			break
		}
		p.skipSeparators()
		val := p.partValue()
		if val == "" {
			// Маркер без значения: «7к». Это литера, а не корпус — так
			// пишут, например, «15А» через «15 к». Оставляем как есть,
			// но помечаем, что разбор неполон.
			h.Flags |= FlagUnparsed
			break
		}
		h.Parts = append(h.Parts, HousePart{Kind: kind, Num: val})
	}

	p.skipSeparators()
	if !p.eof() {
		h.Flags |= FlagUnparsed
	}

	if h.IsEmpty() {
		h.Flags |= FlagEmpty
	}
	if h.Kind == KindUnknown && !h.IsEmpty() {
		h.Kind = KindHouse
	}
	return h
}

type parser struct {
	src []rune
	pos int
}

func (p *parser) eof() bool { return p.pos >= len(p.src) }

func (p *parser) peek() rune {
	if p.eof() {
		return 0
	}
	return p.src[p.pos]
}

// skipSeparators пропускает пробелы, точки и запятые. Слэш НЕ разделитель:
// он несёт смысл углового дома («1/18»).
func (p *parser) skipSeparators() {
	for !p.eof() {
		switch p.src[p.pos] {
		case ' ', '.', ',', '\t', '-', '_':
			p.pos++
		default:
			return
		}
	}
}

// hasPrefix сравнивает с ожидаемым словом начиная с текущей позиции.
func (p *parser) hasPrefix(w string) bool {
	r := []rune(w)
	if p.pos+len(r) > len(p.src) {
		return false
	}
	for i, c := range r {
		if p.src[p.pos+i] != c {
			return false
		}
	}
	return true
}

func (p *parser) kindMarker() (HouseKind, bool) {
	for _, m := range kindMarkers {
		if !p.hasPrefix(m.word) {
			continue
		}
		// Маркер засчитывается, только если за ним идёт разделитель или
		// цифра. Иначе «домодедовская» превратилась бы в «дом» + мусор.
		next := p.pos + len([]rune(m.word))
		if next < len(p.src) && isLetter(p.src[next]) {
			continue
		}
		p.pos = next
		return m.kind, true
	}
	return KindUnknown, false
}

func (p *parser) marker() (AddKind, bool) {
	for _, m := range markers {
		if !p.hasPrefix(m.word) {
			continue
		}
		next := p.pos + len([]rune(m.word))
		// Односимвольные «к» и «с» — только перед цифрой, пробелом, точкой
		// или буквой-литерой. Иначе «сад» стал бы строением.
		if len(m.word) == 1 && next < len(p.src) && isLetter(p.src[next]) {
			// «кГ», «сА» — литера корпуса или строения, это допустимо.
			// А вот «кор», «сооруж» уже разобраны выше как длинные маркеры.
			if next+1 < len(p.src) && isLetter(p.src[next+1]) {
				continue
			}
		}
		p.pos = next
		return m.kind, true
	}
	return AddNone, false
}

// baseNumber читает основной номер: цифры, при необходимости через слэш или
// обратный слэш (угловой дом), с возможной буквой-литерой в конце.
//
// Обратный слэш встречается в проде 30 раз («1\18») — это тот же угловой дом,
// набранный при другой раскладке. Приводим к слэшу.
func (p *parser) baseNumber() (string, bool) {
	var b strings.Builder
	var isWord bool

	for {
		switch {
		case p.eof():
			return b.String(), isWord

		case isDigit(p.peek()):
			for !p.eof() && isDigit(p.peek()) {
				b.WriteRune(p.src[p.pos])
				p.pos++
			}

		case p.peek() == '/' || p.peek() == '\\':
			// Слэш значим только между числами.
			if b.Len() == 0 {
				return b.String(), isWord
			}
			save := p.pos
			p.pos++
			p.skipSeparators()
			if p.eof() || !isDigit(p.peek()) {
				p.pos = save
				return b.String(), isWord
			}
			b.WriteByte('/')

		default:
			// Номер прописью: «два», «шесть».
			if w, n, ok := p.wordNumber(); ok {
				b.WriteString(n)
				isWord = true
				_ = w
				continue
			}
			// Одиночная буква сразу после цифр — литера при номере: «15а».
			//
			// «Одиночная» значит, что за ней НЕ продолжается слово. Но за ней
			// вполне может сразу начинаться обозначение части: «107Ак1» — это
			// дом 107 литера А корпус 1. Без этой проверки каноническая форма
			// «10ак5» переставала разбираться в саму себя.
			if b.Len() > 0 && isLetter(p.peek()) && !p.startsMarker() {
				next := p.pos + 1
				standalone := next >= len(p.src) ||
					!isLetter(p.src[next]) ||
					p.startsMarkerAt(next)
				if standalone {
					b.WriteRune(p.src[p.pos])
					p.pos++
					// Литера ЗАВЕРШАЕТ основной номер. «15а» полон, а «15а20»
					// бессмыслен, и цифры после литеры — уже хвост.
					//
					// Без этого правила «0оо0» разбиралось в «0о00»: литера
					// впитывала цифры за собой, а при повторном разборе «о»
					// оказывалась перед цифрой и превращалась в ноль. Нашёл
					// фаззинг.
					return b.String(), isWord
				}
			}
			return b.String(), isWord
		}
	}
}

// startsMarker сообщает, что с текущей позиции начинается обозначение части.
// Нужен, чтобы «7к7» не прочиталось как номер «7к» с хвостом «7».
func (p *parser) startsMarker() bool { return p.startsMarkerAt(p.pos) }

func (p *parser) startsMarkerAt(pos int) bool {
	save := p.pos
	p.pos = pos
	defer func() { p.pos = save }()

	for _, m := range markers {
		if !p.hasPrefix(m.word) {
			continue
		}
		next := pos + len([]rune(m.word))
		if next < len(p.src) && (isDigit(p.src[next]) || p.src[next] == ' ') {
			return true
		}
		if len(m.word) > 1 {
			return true
		}
	}
	return false
}

func (p *parser) wordNumber() (string, string, bool) {
	for w, n := range wordNumbers {
		if p.hasPrefix(w) {
			p.pos += len([]rune(w))
			return w, n, true
		}
	}
	return "", "", false
}

// partValue читает значение части: «1», «1а», «г», «два».
//
// Значение намеренно короткое: цифры плюс НЕ БОЛЕЕ одной буквы-литеры, либо
// одна буква. Жадное чтение букв превращало «10к31ЛЕЧЕБНЫЙК» в корпус
// «31лечебный», хотя корпус там 31, а остальное — примечание, которому в поле
// дома не место (§6.4 инвентаризации: отделения и корпуса пишут в примечания).
func (p *parser) partValue() string {
	var b strings.Builder

	// Номер части прописью: «10к два».
	if _, n, ok := p.wordNumber(); ok {
		b.WriteString(n)
	} else {
		for !p.eof() && isDigit(p.peek()) {
			b.WriteRune(p.src[p.pos])
			p.pos++
		}
	}

	// Одна буква-литера следом, если за ней не продолжается слово.
	if !p.eof() && isLetter(p.peek()) && !p.startsMarker() {
		next := p.pos + 1
		standalone := next >= len(p.src) ||
			!isLetter(p.src[next]) ||
			p.startsMarkerAt(next)
		if standalone {
			b.WriteRune(p.src[p.pos])
			p.pos++
		}
	}

	return b.String()
}

/* ── Вспомогательное ───────────────────────────────────────────── */

func isDigit(r rune) bool  { return r >= '0' && r <= '9' }
func isLetter(r rune) bool { return unicode.IsLetter(r) }

func leadingInt(s string) int {
	n := 0
	for _, r := range s {
		if !isDigit(r) {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}
