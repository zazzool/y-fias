package normalize

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchNorm(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"Тверская улица", "тверская улица"},
		{"Хорошёвское шоссе", "хорошевское шоссе"},
		{"3-я Парковая улица", "3 я парковая улица"},
		{"Саляма Адиля ул.", "саляма адиля ул"},
		{"Вишнёвая улица (СНТ Лесная поляна)", "вишневая улица снт лесная поляна"},
		{"  двойные   пробелы  ", "двойные пробелы"},
		{"Римский-Корсаков", "римский корсаков"},
		{"тире – и — тире", "тире и тире"},

		// «й» не сводится к «и»: решение записано в миграции 0029.
		{"Гвардейская", "гвардейская"},
	}
	for _, c := range cases {
		if got := SearchNorm(c.in); got != c.want {
			t.Errorf("SearchNorm(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

func TestSearchNormIdempotent(t *testing.T) {
	for _, s := range []string{
		"Тверская улица", "Хорошёвское ш.", "3-я Парковая", "  ", "null null",
	} {
		once := SearchNorm(s)
		if twice := SearchNorm(once); once != twice {
			t.Errorf("не идемпотентна: %q -> %q -> %q", s, once, twice)
		}
	}
}

func TestStreetKeyIgnoresTypeAndOrder(t *testing.T) {
	groups := [][]string{
		{"улица Тверская", "Тверская улица", "Тверская ул.", "тверская"},
		{"улица Саляма Адиля", "Саляма Адиля ул.", "саляма адиля", "Адиля Саляма улица"},
		{"3-я Парковая улица", "Парковая 3-я улица"},
		{"Хорошёвское шоссе", "Хорошевское ш."},
		{"Ленинский проспект", "Ленинский пр-кт", "проспект Ленинский"},
		{"8-я Соколиной Горы улица", "8-я улица Соколиной горы"},
	}
	for _, g := range groups {
		want := StreetKey(g[0])
		if want == "" {
			t.Fatalf("пустой ключ у %q", g[0])
		}
		for _, s := range g[1:] {
			if got := StreetKey(s); got != want {
				t.Errorf("StreetKey(%q) = %q, а StreetKey(%q) = %q — должны совпадать",
					s, got, g[0], want)
			}
		}
	}
}

func TestStreetKeyDistinguishesDifferentStreets(t *testing.T) {
	pairs := [][2]string{
		{"Тверская улица", "Тверской бульвар"}, // тип отброшен, но названия разные
		{"1-я Парковая улица", "3-я Парковая улица"},
		{"Гвардейская улица", "Гвардейский переулок"},
	}
	for _, p := range pairs {
		if a, b := StreetKey(p[0]), StreetKey(p[1]); a == b {
			t.Errorf("StreetKey склеила разные улицы: %q и %q -> %q", p[0], p[1], a)
		}
	}
}

// Улица, целиком состоящая из слова-типа, не должна давать пустой ключ:
// иначе все такие сольются в одну.
func TestStreetKeyTypeOnlyName(t *testing.T) {
	for _, s := range []string{"Линия", "Аллея", "улица"} {
		if got := StreetKey(s); got == "" {
			t.Errorf("StreetKey(%q) пустой", s)
		}
	}
}

func TestSplitStreetType(t *testing.T) {
	cases := []struct{ in, name, typ string }{
		{"улица Тверская", "тверская", "улица"},
		{"Тверская ул.", "тверская", "улица"},
		{"Ленинский пр-кт", "ленинский", "проспект"},
		{"Хорошёвское шоссе", "хорошевское", "шоссе"},
		{"Саляма Адиля", "саляма адиля", ""},
	}
	for _, c := range cases {
		name, typ := SplitStreetType(c.in)
		if name != c.name || typ != c.typ {
			t.Errorf("SplitStreetType(%q) = (%q, %q), ожидалось (%q, %q)",
				c.in, name, typ, c.name, c.typ)
		}
	}
}

// TestGolden_NoNewCollisions — главный тест этапа 1.1.
//
// Список типов улиц расширен с 27 токенов до 47 (добавлены «тер», «просека»,
// «спуск», «тракт» и сокращения вроде «бул», «шос», «мкрн»). Расширение
// ОПАСНО: лишний токен может склеить две разные улицы в один ключ, а значит
// объединить два разных адреса.
//
// Тест берёт 10 694 реальные улицы прода и проверяет, что новый список не
// породил ни одной склейки, которой не было бы у эталонного street_key().
func TestGolden_NoNewCollisions(t *testing.T) {
	rows := loadGolden(t, "street_keys.csv")

	// legacyKey -> множество наших ключей. Если у одного нашего ключа
	// оказались улицы с РАЗНЫМИ эталонными ключами, мы что-то склеили.
	ourToLegacy := map[string]map[string]bool{}
	for _, r := range rows {
		street, legacy := r[0], r[1]
		our := StreetKey(street)
		if ourToLegacy[our] == nil {
			ourToLegacy[our] = map[string]bool{}
		}
		ourToLegacy[our][legacy] = true
	}

	var collisions int
	for our, legacies := range ourToLegacy {
		if len(legacies) <= 1 {
			continue
		}
		collisions++
		if collisions <= 10 {
			keys := make([]string, 0, len(legacies))
			for k := range legacies {
				keys = append(keys, k)
			}
			t.Errorf("склейка: наш ключ %q объединил эталонные %s",
				our, strings.Join(keys, " | "))
		}
	}
	if collisions > 0 {
		t.Fatalf("новых склеек: %d — расширенный список типов улиц объединяет "+
			"разные улицы, см. вывод выше", collisions)
	}
}

// TestGolden_MergesLegacyDuplicates — обратная сторона: расширенный список
// должен склеивать написания, которые эталон РАЗЛИЧАЛ ошибочно. Тест не
// требует конкретного числа, а печатает найденное — материал для ревью.
func TestGolden_MergesLegacyDuplicates(t *testing.T) {
	rows := loadGolden(t, "street_keys.csv")

	legacyToOur := map[string]map[string]bool{}
	for _, r := range rows {
		street, legacy := r[0], r[1]
		if legacyToOur[legacy] == nil {
			legacyToOur[legacy] = map[string]bool{}
		}
		legacyToOur[legacy][StreetKey(street)] = true
	}

	var split int
	for legacy, ours := range legacyToOur {
		if len(ours) > 1 {
			split++
			if split <= 5 {
				t.Logf("эталонный ключ %q распался на %d наших", legacy, len(ours))
			}
		}
	}
	if split > 0 {
		t.Errorf("наш ключ РАЗДЕЛИЛ %d эталонных групп — так быть не должно: "+
			"мы отбрасываем больше токенов, а не меньше", split)
	}
}

func loadGolden(t *testing.T, name string) [][]string {
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
	return rows[1:] // без заголовка
}
