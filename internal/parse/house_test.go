package parse

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIssue_8_2_HouseIdentityIsFullCanonical — регресс на аварию, с которой
// начался разбор всей адресной подсистемы.
//
// Диспетчер вводил дом «7к7», а сохранялся другой номер (карта 2610837).
// Причина: совпадением считалось равенство ВЕДУЩЕГО ЧИСЛА, и «7к7»
// приклеивался к существующему «7к1» на той же улице.
//
// Правило: тождество — только полное равенство канонической формы.
func TestIssue_8_2_HouseIdentityIsFullCanonical(t *testing.T) {
	same := [][]string{
		{"7к7", "7 к 7", "7К7", "7 корпус 7", "7корп7", "7 корп. 7"},
		{"10с4", "10 с 4", "10 строение 4", "10стр4", "10 стр. 4"},
		{"вл15", "владение 15", "влад 15", "вл 15"},
		{"10к2с4", "10с4к2"}, // порядок частей не значим
	}
	for _, g := range same {
		want := ParseHouse(g[0]).Canonical()
		if want == "" {
			t.Fatalf("пустая каноническая форма у %q", g[0])
		}
		for _, s := range g[1:] {
			if got := ParseHouse(s).Canonical(); got != want {
				t.Errorf("%q -> %q, а %q -> %q — должны совпадать", s, got, g[0], want)
			}
		}
	}

	// Главное правило: разные дома остаются разными.
	distinct := []string{"7", "7к1", "7к7", "7с7", "7к7с1", "70", "7а"}
	seen := map[string]string{}
	for _, s := range distinct {
		c := ParseHouse(s).Canonical()
		if prev, ok := seen[c]; ok {
			t.Errorf("СКЛЕЙКА РАЗНЫХ ДОМОВ: %q и %q оба дают %q", prev, s, c)
		}
		seen[c] = s
	}

	// И отдельно — ровно та пара из аварии.
	if a, b := ParseHouse("7к7").Canonical(), ParseHouse("7к1").Canonical(); a == b {
		t.Fatalf("«7к7» и «7к1» признаны одним домом (%q) — регресс §8.2", a)
	}

	// При этом ведущее число у них совпадает: именно поэтому оно не годится
	// как критерий тождества, а только как предфильтр.
	if ParseHouse("7к7").NumN != ParseHouse("7к1").NumN {
		t.Fatal("предпосылка теста неверна: ведущие числа должны совпадать")
	}
}

func TestInvariant_UserInputNeverMutated(t *testing.T) {
	inputs := []string{
		"7к7", " 7 к 7 ", "7r7", "З2к1", "ШЕсТЬк2", "null", "",
		"вл15А", "1/18кБ", "мусор ###", "10с4кссК",
	}
	for _, s := range inputs {
		if got := ParseHouse(s).Raw; got != s {
			t.Errorf("ввод изменён: %q -> %q", s, got)
		}
		if got := ParseHouse(s).String(); got != s {
			t.Errorf("String() вернул не исходный ввод: %q -> %q", s, got)
		}
	}
}

func TestParseHouseStructure(t *testing.T) {
	cases := []struct {
		in      string
		num     string
		corpus  string
		build   string
		kind    HouseKind
		flags   Flags
		canonic string
	}{
		{in: "7", num: "7", kind: KindHouse, canonic: "7"},
		{in: "7к7", num: "7", corpus: "7", kind: KindHouse, canonic: "7к7"},
		{in: "10с4", num: "10", build: "4", kind: KindHouse, canonic: "10с4"},
		{in: "вл15", num: "15", kind: KindOwnership, canonic: "вл15"},
		{in: "владение 15", num: "15", kind: KindOwnership, canonic: "вл15"},
		{in: "15а", num: "15а", kind: KindHouse, canonic: "15а"},
		{in: "1/18", num: "1/18", kind: KindHouse, flags: FlagCorner, canonic: "1/18"},
		{in: `1\18`, num: "1/18", kind: KindHouse, flags: FlagCorner, canonic: "1/18"},
		{in: "2/44кг", num: "2/44", corpus: "г", kind: KindHouse, flags: FlagCorner, canonic: "2/44кг"},
		{in: "10с4к2", num: "10", corpus: "2", build: "4", kind: KindHouse, canonic: "10к2с4"},
	}
	for _, c := range cases {
		h := ParseHouse(c.in)
		if h.Num != c.num {
			t.Errorf("ParseHouse(%q).Num = %q, ожидалось %q", c.in, h.Num, c.num)
		}
		if h.Corpus() != c.corpus {
			t.Errorf("ParseHouse(%q).Corpus() = %q, ожидалось %q", c.in, h.Corpus(), c.corpus)
		}
		if h.Building() != c.build {
			t.Errorf("ParseHouse(%q).Building() = %q, ожидалось %q", c.in, h.Building(), c.build)
		}
		if h.Kind != c.kind {
			t.Errorf("ParseHouse(%q).Kind = %d, ожидалось %d", c.in, h.Kind, c.kind)
		}
		if c.flags != 0 && !h.Flags.Has(c.flags) {
			t.Errorf("ParseHouse(%q).Flags = %b, ожидался флаг %b", c.in, h.Flags, c.flags)
		}
		if got := h.Canonical(); got != c.canonic {
			t.Errorf("ParseHouse(%q).Canonical() = %q, ожидалось %q", c.in, got, c.canonic)
		}
	}
}

// Гомоглифы и раскладка распознаются, но НЕ подставляются молча:
// результат обязан нести флаг.
func TestHomoglyphsAreFlaggedNotSilent(t *testing.T) {
	cases := []struct{ in, canonic string }{
		{"7r7", "7к7"},   // латинская раскладка: r -> к
		{"З2к1", "32к1"}, // кириллическая З на месте цифры 3
		{"7c7", "7с7"},   // латинская c -> кириллическая с (строение)
	}
	for _, c := range cases {
		h := ParseHouse(c.in)
		if !h.Flags.Has(FlagHomoglyph) {
			t.Errorf("ParseHouse(%q): флаг FlagHomoglyph не выставлен", c.in)
		}
		if h.Raw != c.in {
			t.Errorf("ParseHouse(%q): ввод изменён на %q", c.in, h.Raw)
		}
		if got := h.Canonical(); got != c.canonic {
			t.Errorf("ParseHouse(%q).Canonical() = %q, ожидалось %q", c.in, got, c.canonic)
		}
	}

	// «Зорге» — не число: буква З вне числового окружения не трогается.
	if h := ParseHouse("Зорге"); h.Flags.Has(FlagHomoglyph) {
		t.Error("«Зорге»: З ошибочно принята за цифру")
	}
}

// Номера прописью — след обхода аварии §8.2. Разбираем, но помечаем.
func TestWordNumbersAreFlagged(t *testing.T) {
	for _, s := range []string{"два", "ШЕсТЬк2", "ТРИк1", "восемь"} {
		h := ParseHouse(s)
		if !h.Flags.Has(FlagWordNumber) {
			t.Errorf("ParseHouse(%q): флаг FlagWordNumber не выставлен (флаги %b)", s, h.Flags)
		}
	}
	// «ШЕсТЬк2» и «6к2» — один дом.
	if a, b := ParseHouse("ШЕсТЬк2").Canonical(), ParseHouse("6к2").Canonical(); a != b {
		t.Errorf("«ШЕсТЬк2» -> %q, «6к2» -> %q — должны совпадать", a, b)
	}
}

func TestGarbageInput(t *testing.T) {
	for _, s := range []string{"", "   ", "null", "###", "..."} {
		h := ParseHouse(s)
		if h.Raw != s {
			t.Errorf("ввод изменён: %q -> %q", s, h.Raw)
		}
		if h.Canonical() != "" && h.NumN == 0 && !h.Flags.Has(FlagUnparsed) {
			t.Errorf("мусор %q разобран как %q без флага", s, h.Canonical())
		}
	}
}

// Каноническая форма должна быть неподвижной точкой: повторный разбор
// собственного вывода не меняет результат.
func TestCanonicalIsIdempotent(t *testing.T) {
	rows := loadHouses(t)
	var broken int
	for _, raw := range rows {
		once := ParseHouse(raw).Canonical()
		if once == "" {
			continue
		}
		twice := ParseHouse(once).Canonical()
		if once != twice {
			broken++
			if broken <= 15 {
				t.Errorf("не идемпотентно: %q -> %q -> %q", raw, once, twice)
			}
		}
	}
	if broken > 0 {
		t.Fatalf("неидемпотентных написаний: %d из %d", broken, len(rows))
	}
}

// TestGolden_AllRealHousesParse — все 2 033 написания с прода.
// Требование мягкое и намеренно такое: не «разобрать правильно», а «не
// потерять ввод и не упасть». Правильность проверяется точечными тестами выше.
func TestGolden_AllRealHousesParse(t *testing.T) {
	rows := loadHouses(t)
	t.Logf("написаний для разбора: %d", len(rows))

	var (
		empty, unparsed, word, homo, corner int
	)
	for _, raw := range rows {
		h := ParseHouse(raw)
		if h.Raw != raw {
			t.Fatalf("ввод изменён: %q -> %q", raw, h.Raw)
		}
		switch {
		case h.Flags.Has(FlagEmpty):
			empty++
		case h.Flags.Has(FlagUnparsed):
			unparsed++
			if unparsed <= 12 {
				t.Logf("неразобранный хвост: %q -> num=%q parts=%v", raw, h.Num, h.Parts)
			}
		}
		if h.Flags.Has(FlagWordNumber) {
			word++
		}
		if h.Flags.Has(FlagHomoglyph) {
			homo++
		}
		if h.Flags.Has(FlagCorner) {
			corner++
		}
	}

	t.Logf("пустых %d, с хвостом %d, прописью %d, с гомоглифами %d, угловых %d",
		empty, unparsed, word, homo, corner)

	// Порог намеренно мягкий: 10% от РАЗЛИЧНЫХ написаний.
	//
	// Остаток — не дефект грамматики, а грязь в данных. Это записи вида
	// «10к1ЛЕЧЕБНЫЙ», «10 1 ЛЕЧЕБНЫЙ КОРПУС», «10к1ХИРУРГИЧЕскИЙ»: название
	// корпуса больницы, попавшее в поле номера дома. Тот же класс загрязнения
	// §6.4 инвентаризации уже вычищали из поля квартиры — миграция 0040
	// перенесла 1 848 таких значений в примечания.
	//
	// Правильное поведение для них — именно флаг, а не выдуманный разбор:
	// сервис сообщает интерфейсу «здесь есть лишний текст», и человек решает.
	// При этом тождество работает: «10к1ЛЕЧЕБНЫЙ» и «10к1» — один дом.
	if limit := len(rows) / 10; unparsed > limit {
		t.Errorf("неразобранных %d при пороге %d (10%%) — проверьте, "+
			"не деградировала ли грамматика", unparsed, limit)
	}
}

// Запись с неразобранным хвостом обязана оставаться пригодной: номер дома и
// корпус из неё извлекаются, тождество работает. Хвост — примечание, а не
// повод отказать в разборе.
func TestUnparsedTailStillYieldsIdentity(t *testing.T) {
	cases := []struct{ in, canonic string }{
		{"10к1ЛЕЧЕБНЫЙ", "10к1"},
		{"10к1 лечебный", "10к1"},
		{"10к1ХИРУРГИЧЕскИЙ", "10к1"},
		{"10 стр 1лечебный", "10с1"},
	}
	for _, c := range cases {
		h := ParseHouse(c.in)
		if !h.Flags.Has(FlagUnparsed) {
			t.Errorf("ParseHouse(%q): ожидался FlagUnparsed", c.in)
		}
		if got := h.Canonical(); got != c.canonic {
			t.Errorf("ParseHouse(%q).Canonical() = %q, ожидалось %q", c.in, got, c.canonic)
		}
	}

	// И главное: запись с примечанием и запись без него — один дом.
	if a, b := ParseHouse("10к1ЛЕЧЕБНЫЙ").Canonical(), ParseHouse("10к1").Canonical(); a != b {
		t.Errorf("«10к1ЛЕЧЕБНЫЙ» -> %q, «10к1» -> %q — должен быть один дом", a, b)
	}
}

// Разные написания одного дома обязаны схлопываться, а разные дома — нет.
// Тест считает, сколько групп даёт каноническая форма на реальных данных.
func TestGolden_CanonicalCollapsesSpellings(t *testing.T) {
	rows := loadHouses(t)

	byCanon := map[string][]string{}
	for _, raw := range rows {
		c := ParseHouse(raw).Canonical()
		if c == "" {
			continue
		}
		byCanon[c] = append(byCanon[c], raw)
	}

	var collapsed int
	for c, group := range byCanon {
		if len(group) > 1 {
			collapsed++
			if collapsed <= 10 {
				t.Logf("%q <- %s", c, strings.Join(group, " | "))
			}
		}
	}
	t.Logf("канонических форм %d из %d написаний; схлопнуто групп %d",
		len(byCanon), len(rows), collapsed)

	if len(byCanon) >= len(rows) {
		t.Error("каноническая форма не схлопнула ни одного написания — " +
			"это подозрительно: в проде заведомо есть «7 к 7» и «7к7»")
	}
}

func loadHouses(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "houses.csv")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("golden-корпус недоступен (%v). Выгрузить: make golden", err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows[1:] {
		if row[0] != "" {
			out = append(out, row[0])
		}
	}
	return out
}

func FuzzParseHouse(f *testing.F) {
	for _, s := range []string{"7к7", "10с4", "вл15", "1/18", "З2к1", "", "null", "ШЕсТЬк2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		h := ParseHouse(s)
		if h.Raw != s {
			t.Fatalf("ввод изменён: %q -> %q", s, h.Raw)
		}
		// Каноническая форма должна быть устойчива.
		c := h.Canonical()
		if c != "" && ParseHouse(c).Canonical() != c {
			t.Fatalf("неидемпотентно: %q -> %q -> %q", s, c, ParseHouse(c).Canonical())
		}
	})
}

func BenchmarkParseHouse(b *testing.B) {
	inputs := []string{"7к7", "10 строение 4", "вл15А", "1/18кБ", "10с4к2"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = ParseHouse(inputs[i%len(inputs)])
	}
}
