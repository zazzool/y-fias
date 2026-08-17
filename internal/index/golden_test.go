package index

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/zazzool/y-fias/internal/normalize"
)

// loadGoldenSnapshot собирает снапшот из выгрузки прода: 10 694 адреса дают
// список улиц, freq.csv — частоты вызовов.
func loadGoldenSnapshot(t testing.TB) *Snapshot {
	t.Helper()

	addrRows := readCSV(t, "addresses.csv")
	freqRows := readCSV(t, "freq.csv")

	// address_id -> вызовов
	callsByAddr := make(map[string]uint32, len(freqRows))
	for _, r := range freqRows {
		if len(r) < 2 {
			continue
		}
		n, _ := strconv.Atoi(r[1])
		callsByAddr[r[0]] = uint32(n)
	}

	// Улица — уникальна по ключу; вызовы суммируются по всем её домам.
	type agg struct {
		st    Street
		calls uint32
	}
	byKey := map[string]*agg{}

	for _, r := range addrRows {
		if len(r) < 7 {
			continue
		}
		id, region, district, street := r[0], r[1], r[2], r[3]
		if street == "" {
			continue
		}

		key := normalize.StreetKey(street)
		if key == "" {
			continue
		}

		a, ok := byKey[key]
		if !ok {
			name, typ := normalize.SplitStreetType(street)
			a = &agg{st: Street{
				Name:     name,
				Type:     typ,
				Key:      key,
				Region:   region,
				District: district,
			}}
			byKey[key] = a
		}
		a.calls += callsByAddr[id]
	}

	streets := make([]Street, 0, len(byKey))
	for _, a := range byKey {
		a.st.Calls = a.calls
		streets = append(streets, a.st)
	}

	return Build(streets, 1)
}

func readCSV(t testing.TB, name string) [][]string {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", name)
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
	if len(rows) < 2 {
		t.Fatalf("%s пуст", path)
	}
	return rows[1:]
}

func TestGolden_SnapshotBuilds(t *testing.T) {
	s := loadGoldenSnapshot(t)
	t.Logf("улиц в снапшоте: %d", s.Len())

	if s.Len() < 500 {
		t.Fatalf("подозрительно мало улиц: %d", s.Len())
	}

	var withCalls int
	for i := 0; i < s.Len(); i++ {
		if s.Street(uint32(i)).Calls > 0 {
			withCalls++
		}
	}
	t.Logf("улиц с непустой частотой вызовов: %d", withCalls)
	if withCalls == 0 {
		t.Error("частоты вызовов не загрузились — ранжирование лишится " +
			"главного сигнала")
	}
}

// TestGolden_ExactMatchAlwaysFirst — базовое требование: если диспетчер ввёл
// название улицы полностью, она обязана быть первой подсказкой.
func TestGolden_ExactMatchAlwaysFirst(t *testing.T) {
	s := loadGoldenSnapshot(t)

	var checked, failed int
	for i := 0; i < s.Len(); i++ {
		st := s.Street(uint32(i))
		if st.Name == "" {
			continue
		}
		checked++

		res := s.Suggest(Query{Text: st.Full(), Limit: 5})
		if len(res) == 0 {
			failed++
			if failed <= 10 {
				t.Errorf("нет подсказок на точный ввод %q", st.Full())
			}
			continue
		}
		if res[0].Street.Key != st.Key {
			failed++
			if failed <= 10 {
				t.Errorf("на ввод %q первой оказалась %q",
					st.Full(), res[0].Street.Full())
			}
		}
	}
	t.Logf("проверено %d улиц, промахов %d", checked, failed)
	if failed > 0 {
		t.Fatalf("точное совпадение не на первом месте: %d случаев", failed)
	}
}

// TestGolden_LettersToCorrectSuggestion — целевая метрика полезности:
// сколько символов названия надо ввести, чтобы нужная улица попала в первые
// пять подсказок.
//
// Метрика считается ДВАЖДЫ.
//
// Равномерная — каждая улица весит одинаково. Годится как страж регрессии,
// но плохо описывает работу диспетчера.
//
// Взвешенная по вызовам — улица весит столько, сколько на неё приходится
// вызовов. Это и есть реальный опыт: «Большая Сухаревская площадь» с 580
// вызовами вводится несоизмеримо чаще, чем улица с одним.
//
// О сравнении с конкурентами. Замер на 3 591 улице (docs/01-competitors.md
// §1) дал 2.48 у Ahunter, но он шёл по 406 городам, где названия в основном
// уникальны. Наш корпус — только Москва, и он состязательный: на ввод «2й»
// приходится больше восьми улиц вида «2-й ... проезд», а «N-я Парковая» их
// шестнадцать. Никакое ранжирование не поставит нужную первой, пока человек
// не наберёт различающую часть. Поэтому равномерные числа напрямую не
// сопоставимы, и порог здесь взят из измеренной реальности, а не из чужого
// замера.
func TestGolden_LettersToCorrectSuggestion(t *testing.T) {
	s := loadGoldenSnapshot(t)

	const maxLetters = 20
	var (
		total     int
		sum       int
		never     int
		byLetter  = map[int]int{}
		weightSum float64
		weighted  float64
	)

	for i := 0; i < s.Len(); i++ {
		st := s.Street(uint32(i))
		runes := []rune(normalize.SearchNorm(st.Name))
		if len(runes) < 3 {
			continue
		}
		total++

		found := -1
		for n := 1; n <= min(len(runes), maxLetters); n++ {
			res := s.Suggest(Query{Text: string(runes[:n]), Limit: 5})
			for _, r := range res {
				if r.Street.Key == st.Key {
					found = n
					break
				}
			}
			if found > 0 {
				break
			}
		}

		if found < 0 {
			never++
			continue
		}
		sum += found
		byLetter[found]++

		// Вес — число вызовов. Улицы без вызовов учитываются с весом 1,
		// иначе они выпали бы из метрики совсем.
		w := float64(max(st.Calls, 1))
		weightSum += w
		weighted += w * float64(found)
	}

	if total == 0 || weightSum == 0 {
		t.Fatal("нечего измерять")
	}
	avg := float64(sum) / float64(total-never)
	avgWeighted := weighted / weightSum

	t.Logf("улиц %d, не найдено ни на одном префиксе %d", total, never)
	for n := 1; n <= 8; n++ {
		if c := byLetter[n]; c > 0 {
			t.Logf("  %2d букв: %5d улиц", n, c)
		}
	}
	t.Logf("РАВНОМЕРНО:      %.2f буквы", avg)
	t.Logf("ПО ЧАСТОТЕ ВЫЗОВОВ: %.2f буквы  <- реальный опыт диспетчера", avgWeighted)

	// Пороги — стражи регрессии. Подняты выше текущих значений ровно
	// настолько, чтобы ловить ухудшение, а не шум.
	if avg > 2.9 {
		t.Errorf("равномерная метрика %.2f при пороге 2.9", avg)
	}
	if avgWeighted > 2.3 {
		t.Errorf("взвешенная метрика %.2f при пороге 2.3", avgWeighted)
	}
	if never > total/50 {
		t.Errorf("не найдено ни на одном префиксе: %d при пороге %d (2%%)",
			never, total/50)
	}
}

// TestGolden_TypoTolerance — устойчивость к опечаткам на реальных названиях.
//
// Опечатки порождаются из самих улиц корпуса, а не выдумываются: попытка
// написать тест на «Тверскую» провалилась ровно потому, что в базе
// психиатрической неотложки такой улицы нет вовсе. Проверять надо на тех
// данных, которые есть.
func TestGolden_TypoTolerance(t *testing.T) {
	s := loadGoldenSnapshot(t)

	// Четыре класса опечаток: пропуск, дубль, перестановка, замена.
	mutations := []struct {
		name string
		fn   func([]rune) string
	}{
		{"пропуск буквы", func(r []rune) string {
			i := len(r) / 2
			return string(append(append([]rune{}, r[:i]...), r[i+1:]...))
		}},
		{"лишняя буква", func(r []rune) string {
			i := len(r) / 2
			out := append([]rune{}, r[:i]...)
			out = append(out, r[i])
			return string(append(out, r[i:]...))
		}},
		{"перестановка", func(r []rune) string {
			i := len(r) / 2
			out := append([]rune{}, r...)
			out[i], out[i-1] = out[i-1], out[i]
			return string(out)
		}},
		{"замена буквы", func(r []rune) string {
			i := len(r) / 2
			out := append([]rune{}, r...)
			if out[i] == 'а' {
				out[i] = 'о'
			} else {
				out[i] = 'а'
			}
			return string(out)
		}},
	}

	for _, m := range mutations {
		var checked, found int
		for i := 0; i < s.Len(); i++ {
			st := s.Street(uint32(i))
			runes := []rune(normalize.SearchNorm(st.Name))
			// Короткие названия при опечатке становятся неузнаваемы —
			// это ограничение метода, а не индекса.
			if len(runes) < 7 {
				continue
			}
			checked++

			typo := m.fn(runes)
			for _, r := range s.Suggest(Query{Text: typo, Limit: 10}) {
				if r.Street.Key == st.Key {
					found++
					break
				}
			}
		}

		if checked == 0 {
			t.Fatalf("%s: нечего проверять", m.name)
		}
		rate := float64(found) / float64(checked)
		t.Logf("%-16s найдено %d из %d (%.1f%%)", m.name, found, checked, rate*100)

		if rate < 0.85 {
			t.Errorf("%s: найдено только %.1f%% при пороге 85%%", m.name, rate*100)
		}
	}
}

// Бенчмарки берут запросы ИЗ КОРПУСА. Первая версия использовала «Тверскую»,
// которой в базе психиатрической неотложки нет вовсе, и оба замера на самом
// деле мерили худший случай — проход до нечёткого слоя.

func BenchmarkSuggestExact(b *testing.B) {
	s := loadGoldenSnapshot(b)
	q := s.Street(0).Full()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Suggest(Query{Text: q, Limit: 10})
	}
}

func BenchmarkSuggestPrefix(b *testing.B) {
	s := loadGoldenSnapshot(b)

	// Двухбуквенные префиксы реальных улиц — самый частый ввод.
	queries := make([]string, 0, 16)
	for i := 0; i < s.Len() && len(queries) < 16; i += s.Len() / 16 {
		r := []rune(normalize.SearchNorm(s.Street(uint32(i)).Name))
		if len(r) >= 3 {
			queries = append(queries, string(r[:3]))
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		_ = s.Suggest(Query{Text: queries[i%len(queries)], Limit: 10})
	}
}

// BenchmarkSuggestWorstCase — запрос, которого в справочнике нет: проходит
// все четыре слоя, включая подстрочный линейный проход и нечёткий.
func BenchmarkSuggestWorstCase(b *testing.B) {
	s := loadGoldenSnapshot(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_ = s.Suggest(Query{Text: "щукинскаяяя", Limit: 10})
	}
}
