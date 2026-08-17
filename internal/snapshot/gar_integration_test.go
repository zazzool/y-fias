//go:build integration

package snapshot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
)

// Проверки на реальном снапшоте ГАР. Собрать его:
//
//	make snapshot
//
// Берётся полный снапшот, если он есть, иначе московский: проверки одинаково
// осмысленны на обоих, а требовать одиннадцатиминутной сборки ради запуска
// тестов не стоит.
func loadGar(t *testing.T) ([]index.Street, Header) {
	t.Helper()

	var lastErr error
	for _, name := range []string{"gar.yfsnap", "moscow.yfsnap"} {
		path := filepath.Join("..", "..", "data", name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		d, hdr, err := Read(path)
		if err != nil {
			lastErr = err
			continue
		}
		t.Logf("снапшот: %s", name)
		return d.Streets, hdr
	}
	if lastErr != nil {
		t.Fatalf("чтение снапшота: %v", lastErr)
	}
	t.Skip("снапшот ГАР не собран. Собрать: make snapshot")
	return nil, Header{}
}

func TestGar_DataQuality(t *testing.T) {
	streets, hdr := loadGar(t)
	t.Logf("улиц %d, версия ГАР %d", len(streets), hdr.Version)

	var (
		noName, noType, noDistrict int
		types                      = map[string]int{}
	)
	for _, s := range streets {
		if s.Name == "" {
			noName++
		}
		if s.Type == "" {
			noType++
		} else {
			types[s.Type]++
		}
		if s.District == "" {
			noDistrict++
		}
	}

	t.Logf("без названия %d, без типа %d, без района %d", noName, noType, noDistrict)
	t.Logf("типов улиц: %d", len(types))

	if noName > 0 {
		t.Errorf("объектов без названия: %d — в ГАР такого быть не должно", noName)
	}
	// Район нужен для подсказки ПНД. Часть объектов его законно не имеет
	// (сам город, внутригородские территории верхнего уровня), но их немного.
	if limit := len(streets) / 10; noDistrict > limit {
		t.Errorf("без района %d при пороге %d (10%%) — вероятно, "+
			"иерархия разрешилась неверно", noDistrict, limit)
	}
}

// Ключевая проверка ингеста: типы улиц из ГАР («ул», «пер», «ш») обязаны
// разворачиваться в канонические, иначе ключ улицы из справочника не совпадёт
// с ключом из пользовательского ввода.
func TestGar_StreetTypesAreCanonical(t *testing.T) {
	streets, _ := loadGar(t)

	bad := map[string]int{}
	var examples []string
	var degenerate int

	for _, s := range streets {
		if s.Type == "" {
			continue
		}
		// Названия, целиком состоящие из слов-типов («Парковая», «Линия»),
		// пропускаем: для них StreetKey намеренно возвращает название как
		// есть, иначе ключ был бы пустым и все такие улицы слились бы в одну.
		if normalize.StreetKey(s.Name) == normalize.SearchNorm(s.Name) &&
			allTypeTokens(s.Name) {
			degenerate++
			continue
		}

		// Требование формулируется через тот же конвейер, который строит
		// ключ: тип обязан ИСЧЕЗАТЬ из ключа. Проверять IsStreetType по
		// SearchNorm недостаточно — «м-ко» распадается на два токена, и
		// проверка врёт, хотя ключ строится верно.
		if normalize.StreetKey(s.Full()) != normalize.StreetKey(s.Name) {
			bad[s.Type]++
			if len(examples) < 10 {
				examples = append(examples, s.Full())
			}
		}
	}

	t.Logf("названий из одних слов-типов: %d (пропущены)", degenerate)

	if len(bad) > 0 {
		for typ, n := range bad {
			t.Logf("тип %q не исчезает из ключа — %d объектов", typ, n)
		}
		for _, e := range examples {
			t.Logf("  пример: %q -> ключ %q", e, normalize.StreetKey(e))
		}
		t.Errorf("типов, не входящих в словарь: %d. Ключи из ГАР и из ввода "+
			"диспетчера не совпадут", len(bad))
	}
}

// allTypeTokens — состоит ли название целиком из слов-типов.
func allTypeTokens(name string) bool {
	fields := strings.Fields(normalize.SearchNorm(name))
	if len(fields) == 0 {
		return false
	}
	for _, f := range fields {
		if !normalize.IsStreetType(f) {
			return false
		}
	}
	return true
}

// Наши 10 694 адреса должны находиться в справочнике ГАР. Совпадение не будет
// стопроцентным: справочник ФНС не содержит части реальных строений (§8.7
// описи), но улицы обязаны находиться почти все.
func TestGar_CoversOurStreets(t *testing.T) {
	streets, _ := loadGar(t)

	garKeys := make(map[string]bool, len(streets))
	for _, s := range streets {
		garKeys[normalize.StreetKey(s.Full())] = true
	}

	ours := readOurStreets(t)
	var found, missing int
	var examples []string
	for _, s := range ours {
		if garKeys[normalize.StreetKey(s)] {
			found++
			continue
		}
		missing++
		if len(examples) < 15 {
			examples = append(examples, s)
		}
	}

	rate := float64(found) / float64(found+missing)
	t.Logf("наших улиц %d, найдено в ГАР %d (%.1f%%)", found+missing, found, rate*100)
	for _, e := range examples {
		t.Logf("  нет в ГАР: %q", e)
	}

	if rate < 0.80 {
		t.Errorf("покрытие %.1f%% при пороге 80%% — вероятно, ингест "+
			"или нормализация работают неверно", rate*100)
	}
}

// TestGar_DistrictsMatchOurs — проверка, от которой зависит подсказка ПНД.
//
// Связь «район -> ПНД» устанавливается по ТЕКСТУ названия района (§2.2 описи:
// LEFT JOIN pnds ON pnds.dis = addresses.district). Если названия районов из
// ГАР не совпадут с нашими, подсказка ПНД не сработает ни разу, притом что
// формально всё «загрузилось».
//
// В выгрузке район называется «муниципальный округ Басманный», у нас —
// «Басманный», поэтому ингест снимает служебный префикс. Тест сторожит именно
// это преобразование.
func TestGar_DistrictsMatchOurs(t *testing.T) {
	streets, _ := loadGar(t)

	garDistricts := map[string]bool{}
	for _, s := range streets {
		if s.District != "" {
			garDistricts[normalize.SearchNorm(s.District)] = true
		}
	}
	t.Logf("районов в ГАР: %d", len(garDistricts))

	// Сравнивать надо с таблицей pnds, а не со всеми районами из адресов.
	//
	// Первая версия теста брала районы из addresses и показывала 67.6%.
	// Но там лежат и города области на адресах региона 77, и исторические
	// названия — опись прямо говорит (§2.4), что району из нашей базы
	// доверять как единственному источнику нельзя. Вопрос, на который надо
	// ответить, другой: заработает ли связь «район -> ПНД». Её левая часть —
	// именно pnds.dis.
	ours := readPndDistricts(t)
	if len(ours) == 0 {
		t.Skip("справочник ПНД недоступен")
	}

	var found, missing int
	var examples []string
	for d := range ours {
		if garDistricts[normalize.SearchNorm(d)] {
			found++
			continue
		}
		missing++
		if len(examples) < 20 {
			examples = append(examples, d)
		}
	}

	total := found + missing
	rate := float64(found) / float64(total)
	t.Logf("районов в справочнике ПНД %d, совпало с ГАР %d (%.1f%%)", total, found, rate*100)
	for _, e := range examples {
		t.Logf("  нет в ГАР: %q", e)
	}

	if rate < 0.75 {
		t.Errorf("совпало %.1f%% при пороге 75%% — подсказка ПНД не заработает", rate*100)
	}
}

// readPndDistricts читает названия районов из справочника ПНД (148 строк).
func readPndDistricts(t *testing.T) map[string]bool {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "pnds.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("golden-корпус недоступен (%v)", err)
	}

	lines := splitLines(data)
	if len(lines) < 2 {
		return nil
	}
	// Колонка dis — по заголовку, а не по номеру: порядок колонок в проде
	// менялся, и жёсткий индекс однажды уже подвёл бы.
	head := splitCSV(lines[0])
	col := -1
	for i, h := range head {
		if h == "dis" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("в pnds.csv нет колонки dis, есть: %v", head)
	}

	out := map[string]bool{}
	for _, line := range lines[1:] {
		f := splitCSV(line)
		if len(f) > col && f[col] != "" {
			out[f[col]] = true
		}
	}
	return out
}

func splitLines(data []byte) [][]byte {
	var out [][]byte
	var cur []byte
	for _, b := range data {
		switch b {
		case '\n':
			out = append(out, cur)
			cur = nil
		case '\r':
		default:
			cur = append(cur, b)
		}
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// splitCSV — минимальный разбор строки CSV с кавычками.
func splitCSV(line []byte) []string {
	var (
		out    []string
		cur    []byte
		inQuot bool
	)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '"':
			if inQuot && i+1 < len(line) && line[i+1] == '"' {
				cur = append(cur, '"')
				i++
				continue
			}
			inQuot = !inQuot
		case c == ',' && !inQuot:
			out = append(out, string(cur))
			cur = nil
		default:
			cur = append(cur, c)
		}
	}
	return append(out, string(cur))
}

func TestGar_IndexBuildAndSearch(t *testing.T) {
	streets, hdr := loadGar(t)

	start := time.Now()
	snap := index.Build(streets, hdr.Version)
	buildTime := time.Since(start)
	t.Logf("индекс на %d улиц собран за %s", snap.Len(), buildTime.Round(time.Millisecond))

	if buildTime > 10*time.Second {
		t.Errorf("сборка индекса заняла %s — слишком долго для старта сервиса", buildTime)
	}

	// Замер задержки на реальном объёме: именно здесь проверяется бюджет
	// §9.3 архитектуры (p99 <= 5 мс в процессе).
	queries := []string{"твер", "лен", "садовая", "парковая", "мичуринский", "адиля"}
	for _, q := range queries {
		start := time.Now()
		res := snap.Suggest(index.Query{Text: q, Limit: 10})
		el := time.Since(start)
		first := ""
		if len(res) > 0 {
			first = res[0].Street.Label()
		}
		t.Logf("%-14q -> %2d подсказок за %-10s первая: %s", q, len(res), el.Round(time.Microsecond), first)
		if el > 5*time.Millisecond {
			t.Errorf("запрос %q занял %s при бюджете 5 мс", q, el)
		}
	}
}

func readOurStreets(t *testing.T) []string {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "street_keys.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("golden-корпус недоступен (%v)", err)
	}

	var out []string
	var line []byte
	for _, b := range data {
		if b == '\n' {
			if s := parseFirstField(line); s != "" {
				out = append(out, s)
			}
			line = line[:0]
			continue
		}
		if b != '\r' {
			line = append(line, b)
		}
	}
	if len(out) > 0 {
		out = out[1:] // заголовок
	}
	return out
}

func parseFirstField(line []byte) string {
	if len(line) == 0 {
		return ""
	}
	if line[0] == '"' {
		for i := 1; i < len(line); i++ {
			if line[i] == '"' {
				return string(line[1:i])
			}
		}
		return ""
	}
	for i, b := range line {
		if b == ',' {
			return string(line[:i])
		}
	}
	return string(line)
}
