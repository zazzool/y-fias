package parse

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"testing"
)

func TestParseAddress(t *testing.T) {
	cases := []struct {
		in         string
		street     string
		streetType string
		house      string // каноническая форма
		settlement string
		region     string
		district   string
	}{
		{in: "Тверская улица", street: "тверская", streetType: "улица"},
		{in: "улица Тверская, 7", street: "тверская", streetType: "улица", house: "7"},
		{in: "Тверская улица 7к1", street: "тверская", streetType: "улица", house: "7к1"},
		{in: "Тверская улица, д. 7 к. 1", street: "тверская", streetType: "улица", house: "7к1"},
		{in: "Тверская 7 к 1", street: "тверская", house: "7к1"},
		{in: "Саляма Адиля, 2", street: "саляма адиля", house: "2"},
		{in: "ул. Саляма Адиля, д.2", street: "саляма адиля", streetType: "улица", house: "2"},

		// Название, начинающееся с числа: «8» — часть имени, а не дом.
		{in: "улица 8 Марта", street: "8 марта", streetType: "улица"},
		{in: "улица 8 Марта, 5", street: "8 марта", streetType: "улица", house: "5"},
		{in: "8 Марта улица 5", street: "8 марта", streetType: "улица", house: "5"},

		// Порядковые числительные.
		{in: "3-я Парковая улица, 33", street: "3я парковая", streetType: "улица", house: "33"},

		// Владение остаётся частью тождества, «д» — нет.
		{in: "Тверская, вл15", street: "тверская", house: "вл15"},

		// Населённый пункт в скобках — приём из §3.5 описи.
		{
			in:     "Вишнёвая улица (СНТ Лесная поляна, Раменский р-н), 5",
			street: "вишневая", streetType: "улица", house: "5",
			settlement: "СНТ Лесная поляна", district: "раменский",
		},

		// Полный адрес области одной строкой.
		{
			in:     "Московская область, Клинский р-н, пос. Покровка, Лесная улица, д. 3",
			street: "лесная", streetType: "улица", house: "3",
			region:   "50",
			district: "клинский", settlement: "пос. Покровка",
		},
	}

	for _, c := range cases {
		a := ParseAddress(c.in)
		if a.Raw != c.in {
			t.Errorf("ввод изменён: %q -> %q", c.in, a.Raw)
		}
		if a.Street != c.street {
			t.Errorf("ParseAddress(%q).Street = %q, ожидалось %q", c.in, a.Street, c.street)
		}
		if a.StreetType != c.streetType {
			t.Errorf("ParseAddress(%q).StreetType = %q, ожидалось %q", c.in, a.StreetType, c.streetType)
		}
		if got := a.House.Canonical(); got != c.house {
			t.Errorf("ParseAddress(%q).House = %q, ожидалось %q", c.in, got, c.house)
		}
		if c.settlement != "" && a.Settlement != c.settlement {
			t.Errorf("ParseAddress(%q).Settlement = %q, ожидалось %q", c.in, a.Settlement, c.settlement)
		}
		if c.region != "" && a.Region != c.region {
			t.Errorf("ParseAddress(%q).Region = %q, ожидалось %q", c.in, a.Region, c.region)
		}
		if c.district != "" && a.District != c.district {
			t.Errorf("ParseAddress(%q).District = %q, ожидалось %q", c.in, a.District, c.district)
		}
	}
}

// Одноимённые СНТ разных районов обязаны различаться — ради этого §3.5 описи
// вводил контекст в подписи подсказок.
func TestParseAddress_DistinguishesSameNamedSettlements(t *testing.T) {
	a := ParseAddress("Вишнёвая улица (СНТ Лесная поляна, Раменский р-н)")
	b := ParseAddress("Вишнёвая улица (СНТ Ромашка, Ногинский р-н)")

	if a.Key() != b.Key() {
		t.Fatalf("предпосылка: ключ улицы должен совпадать, а он %q и %q", a.Key(), b.Key())
	}
	if a.Settlement == b.Settlement {
		t.Errorf("населённые пункты не различены: оба %q", a.Settlement)
	}
	if a.District == b.District {
		t.Errorf("районы не различены: оба %q", a.District)
	}
}

func TestParseAddress_TailIsPreservedNotDropped(t *testing.T) {
	a := ParseAddress("Тверская улица, 7, подъезд 3, домофон 45")
	if a.Street != "тверская" {
		t.Fatalf("улица разобрана как %q", a.Street)
	}
	if a.Tail == "" {
		t.Error("хвост потерян — он должен возвращаться, а не выбрасываться")
	}
	if a.Quality.Parse != ParseWithTail {
		t.Errorf("Quality.Parse = %d, ожидалось ParseWithTail", a.Quality.Parse)
	}
}

func TestParseAddress_Garbage(t *testing.T) {
	for _, s := range []string{"", "   ", ",,,"} {
		a := ParseAddress(s)
		if a.Raw != s {
			t.Errorf("ввод изменён: %q -> %q", s, a.Raw)
		}
		if a.Quality.Parse != ParseGarbage {
			t.Errorf("ParseAddress(%q).Quality.Parse = %d, ожидалось ParseGarbage", s, a.Quality.Parse)
		}
	}

	// «null null» — 21 такой адрес в проде (§2.4 описи). Падать нельзя.
	a := ParseAddress("null null")
	if a.Raw != "null null" {
		t.Error("ввод изменён")
	}
}

func TestParseAddress_SpansPointAtInput(t *testing.T) {
	in := "Тверская улица, 7к1"
	a := ParseAddress(in)
	if len(a.Spans) == 0 {
		t.Fatal("спаны не построены")
	}
	for _, sp := range a.Spans {
		if sp.From < 0 || sp.To > len(in) || sp.From >= sp.To {
			t.Errorf("некорректный спан %+v для строки длины %d", sp, len(in))
			continue
		}
		if got := in[sp.From:sp.To]; got == "" {
			t.Errorf("спан %+v указывает на пустой фрагмент", sp)
		}
	}
}

// TestGolden_AllRealStreetsParse — все реальные написания улиц с прода.
// Требование: не потерять ввод, не упасть, выделить непустое название.
func TestGolden_AllRealStreetsParse(t *testing.T) {
	streets := loadGoldenStreets(t)
	t.Logf("написаний улиц: %d", len(streets))

	var noStreet, withTail, withHouse int
	for _, s := range streets {
		a := ParseAddress(s)
		if a.Raw != s {
			t.Fatalf("ввод изменён: %q -> %q", s, a.Raw)
		}
		if a.Street == "" {
			noStreet++
			if noStreet <= 10 {
				t.Logf("название не выделено: %q", s)
			}
		}
		if a.Tail != "" {
			withTail++
		}
		if !a.House.IsEmpty() {
			withHouse++
		}
	}
	t.Logf("без названия %d, с хвостом %d, с домом внутри %d", noStreet, withTail, withHouse)

	if limit := len(streets) / 50; noStreet > limit {
		t.Errorf("слишком много строк без названия улицы: %d при пороге %d (2%%)",
			noStreet, limit)
	}
}

func loadGoldenStreets(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "street_keys.csv")
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

func FuzzParseAddress(f *testing.F) {
	for _, s := range []string{
		"Тверская улица, 7к1", "улица 8 Марта", "", "null null",
		"Вишнёвая улица (СНТ Лесная поляна, Раменский р-н), 5",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a := ParseAddress(s)
		if a.Raw != s {
			t.Fatalf("ввод изменён: %q -> %q", s, a.Raw)
		}
		for _, sp := range a.Spans {
			if sp.From < 0 || sp.To > len(s) || sp.From >= sp.To {
				t.Fatalf("спан %+v выходит за границы строки длины %d", sp, len(s))
			}
		}
	})
}

func BenchmarkParseAddress(b *testing.B) {
	inputs := []string{
		"Тверская улица, 7к1",
		"улица 8 Марта, д. 5 стр. 2",
		"Вишнёвая улица (СНТ Лесная поляна, Раменский р-н), 5",
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = ParseAddress(inputs[i%len(inputs)])
	}
}
