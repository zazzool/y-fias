//go:build integration

package geo

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Проверки на реальных границах. Собрать:
//
//	go run ./cmd/yfias-geo -regions=77,50 -out=data/geo.json
func loadReal(t *testing.T) *Index {
	t.Helper()

	path := filepath.Join("..", "..", "data", "geo.json")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("границы не загружены (%v). Загрузить: make geo", err)
	}
	defer f.Close()

	var areas []Area
	if err := json.NewDecoder(f).Decode(&areas); err != nil {
		t.Fatalf("разбор %s: %v", path, err)
	}
	return NewIndex(areas)
}

func TestReal_ControlPoints(t *testing.T) {
	ix := loadReal(t)
	t.Logf("областей: %d", ix.Len())

	// Точки, ответ для которых известен заранее. Ошибка сборки колец не
	// приводит к падению — полигон просто оказывается вывернутым, и проверка
	// начинает молча врать. Ловится только такими точками.
	cases := []struct {
		name string
		pt   Point
		want string
	}{
		{"Красная площадь", Point{Lon: 37.6208, Lat: 55.7539}, "Тверской"},
		{"ГКБ 67, Саляма Адиля 2", Point{Lon: 37.4906, Lat: 55.7761}, "Хорошёво-Мнёвники"},
		{"Останкинская башня", Point{Lon: 37.6117, Lat: 55.8197}, "Останкинский"},
		// Координаты берутся проверенными. Первая версия теста утверждала,
		// что точка 37.6266/55.6739 — это ПКБ №1 в Донском районе; на деле
		// она лежит в Нагорном, и «ошибку» показал не код, а тест.
		{"Нагорный проезд", Point{Lon: 37.6266, Lat: 55.6739}, "Нагорный"},
		{"МГУ", Point{Lon: 37.5286, Lat: 55.7033}, "Раменки"},
		{"Аэропорт Внуково", Point{Lon: 37.2615, Lat: 55.5915}, "Внуково"},
	}

	for _, c := range cases {
		area, n := ix.Locate(c.pt)
		if area == nil {
			t.Errorf("%s: не попала ни в одну область (ожидался %q)", c.name, c.want)
			continue
		}
		if !equalRU(area.Name, c.want) {
			t.Errorf("%s: получено %q, ожидался %q", c.name, area.Name, c.want)
			continue
		}
		if n > 1 {
			t.Logf("%s: %q (подошло областей %d — границы перекрываются)",
				c.name, area.Name, n)
		}
	}
}

// TestReal_DistrictsMatchPnd — проверка, ради которой полигоны и нужны.
//
// Район определяет ПНД. Если названия районов из OSM не совпадут с
// справочником ПНД, точная геометрия окажется бесполезной: связь по тексту
// не установится.
func TestReal_DistrictsMatchPnd(t *testing.T) {
	ix := loadReal(t)

	osmNames := map[string]bool{}
	for _, a := range ix.Areas() {
		osmNames[normRU(a.Name)] = true
	}

	pnd := readPndDistricts(t)
	if len(pnd) == 0 {
		t.Skip("справочник ПНД недоступен")
	}

	var found, missing int
	var examples []string
	for d := range pnd {
		if osmNames[normRU(d)] {
			found++
			continue
		}
		missing++
		if len(examples) < 15 {
			examples = append(examples, d)
		}
	}

	total := found + missing
	rate := float64(found) / float64(total)
	t.Logf("районов в справочнике ПНД %d, совпало с OSM %d (%.1f%%)",
		total, found, rate*100)
	for _, e := range examples {
		t.Logf("  нет в OSM: %q", e)
	}

	// Доля НАЗВАНИЙ мало что говорит: несовпавшие — это поселения Новой
	// Москвы, на которые приходятся единицы адресов. Значение имеет доля
	// АДРЕСОВ, для которых район определится.
	addrTotal, addrMatched := weighByAddresses(t, osmNames)
	if addrTotal > 0 {
		wrate := float64(addrMatched) / float64(addrTotal)
		t.Logf("адресов с районом %d, район есть в OSM у %d (%.1f%%)",
			addrTotal, addrMatched, wrate*100)

		if wrate < 0.90 {
			t.Errorf("район определится лишь для %.1f%% адресов при пороге 90%%",
				wrate*100)
		}
	}

	if rate < 0.75 {
		t.Errorf("совпало %.1f%% названий при пороге 75%%", rate*100)
	}
}

// weighByAddresses считает, для скольких РЕАЛЬНЫХ адресов район найдётся
// в границах OSM.
func weighByAddresses(t *testing.T, osmNames map[string]bool) (total, matched int) {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "addresses.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	for i, l := range lines {
		if i == 0 || l == "" {
			continue
		}
		f := strings.Split(l, ",")
		if len(f) < 3 || f[2] == "" {
			continue
		}
		total++
		if osmNames[normRU(f[2])] {
			matched++
		}
	}
	return total, matched
}

// Границы районов не должны перекрываться: точка обязана попадать ровно в
// одну область своего уровня. Перекрытия в данных OSM встречаются и означают,
// что выбор района станет произвольным.
func TestReal_NoOverlaps(t *testing.T) {
	ix := loadReal(t)

	// Сетка по Москве с шагом примерно в километр.
	var checked, overlapping, outside int
	for lat := 55.55; lat <= 55.92; lat += 0.01 {
		for lon := 37.35; lon <= 37.86; lon += 0.015 {
			checked++
			area, n := ix.Locate(Point{Lon: lon, Lat: lat})
			switch {
			case area == nil:
				outside++
			case n > 1:
				overlapping++
			}
		}
	}

	t.Logf("проверено точек %d, вне районов %d, в перекрытии %d",
		checked, outside, overlapping)

	// Москва не покрывает свой прямоугольник целиком, поэтому «вне районов»
	// нормально. А вот перекрытия — нет.
	if limit := checked / 20; overlapping > limit {
		t.Errorf("точек в перекрытии %d при пороге %d (5%%) — границы OSM "+
			"конфликтуют, выбор района станет произвольным", overlapping, limit)
	}
}

func BenchmarkRealLocate(b *testing.B) {
	path := filepath.Join("..", "..", "data", "geo.json")
	f, err := os.Open(path)
	if err != nil {
		b.Skip(err)
	}
	defer f.Close()

	var areas []Area
	if err := json.NewDecoder(f).Decode(&areas); err != nil {
		b.Fatal(err)
	}
	ix := NewIndex(areas)

	pt := Point{Lon: 37.4906, Lat: 55.7761}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ix.Locate(pt)
	}
}

func TestReal_LocateLatency(t *testing.T) {
	ix := loadReal(t)

	pts := []Point{
		{Lon: 37.6208, Lat: 55.7539},
		{Lon: 37.4906, Lat: 55.7761},
		{Lon: 37.2615, Lat: 55.5915},
		{Lon: 38.5, Lat: 55.5}, // область
	}

	// Замеряется СРЕДНЕЕ по большому числу повторов, а не худшее.
	//
	// Первая версия брала худшее из 800 и показывала 1.0-1.2 мс, из-за чего
	// я едва не начал оптимизировать уже быстрый код. На Windows разрешение
	// таймера и вытеснение планировщиком дают выбросы в миллисекунду на
	// ровном месте: бенчмарк на тех же данных даёт 593 нс. Мерить надо то,
	// что не тонет в шуме.
	const iterations = 20000

	start := time.Now()
	for i := range iterations {
		ix.Locate(pts[i%len(pts)])
	}
	avg := time.Since(start) / iterations

	t.Logf("среднее время поиска области: %s", avg.Round(time.Nanosecond))

	// Определение района идёт на горячем пути подсказки, бюджет §9.3 — 5 мс
	// на весь запрос, поэтому геометрии отводится заведомо меньше.
	if avg > 100*time.Microsecond {
		t.Errorf("поиск области занимает %s при ориентире 100 мкс", avg)
	}
}

/* ── Вспомогательное ───────────────────────────────────────────── */

func normRU(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range strings.ToLower(s) {
		if r == 'ё' {
			r = 'е'
		}
		if r == '-' {
			r = ' '
		}
		out = append(out, r)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

func equalRU(a, b string) bool { return normRU(a) == normRU(b) }

func readPndDistricts(t *testing.T) map[string]bool {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", "pnds.csv")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("golden-корпус недоступен (%v)", err)
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	if len(lines) < 2 {
		return nil
	}

	head := strings.Split(lines[0], ",")
	col := -1
	for i, h := range head {
		if h == "dis" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("в pnds.csv нет колонки dis: %v", head)
	}

	out := map[string]bool{}
	for _, l := range lines[1:] {
		f := strings.Split(l, ",")
		if len(f) > col && f[col] != "" {
			out[f[col]] = true
		}
	}
	return out
}
