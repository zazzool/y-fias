//go:build integration

package snapshot

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
	"github.com/zazzool/y-fias/internal/parse"
)

// Проверки на ПОЛНОМ справочнике: Москва и область, 85 тыс. объектов и
// 2.3 млн домов. Собрать:
//
//	go run ./cmd/yfias-ingest -regions=77,50 -houses -out=data/gar.yfsnap
func loadFull(t *testing.T) (Data, Header) {
	t.Helper()

	path := filepath.Join("..", "..", "data", "gar.yfsnap")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("полный снапшот не собран (%v)", err)
	}
	d, hdr, err := Read(path)
	if err != nil {
		t.Fatalf("чтение: %v", err)
	}
	return d, hdr
}

// TestFull_MemoryAndLatency — проверка бюджетов §1 и §9.3 архитектуры на
// реальном полном объёме, а не на выборке.
func TestFull_MemoryAndLatency(t *testing.T) {
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	d, hdr := loadFull(t)

	buildStart := time.Now()
	snap := index.Build(d.Streets, hdr.Version)
	snap.SetHouses(d.Houses)
	buildTime := time.Since(buildStart)

	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)

	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if used < 0 {
		used = int64(after.HeapAlloc)
	}

	t.Logf("объектов %d, домов %d", snap.Len(), snap.HouseCount())
	t.Logf("память после загрузки и сборки индексов: %.1f МБ", float64(used)/(1<<20))
	t.Logf("сборка индексов: %s", buildTime.Round(time.Millisecond))

	// Бюджет §1: рабочий набор <= 1 ГБ.
	if limit := int64(1) << 30; used > limit {
		t.Errorf("занято %.1f МБ при бюджете 1024 МБ", float64(used)/(1<<20))
	}
	// Старт сервиса не должен растягиваться: снапшот читается и индексируется
	// до того, как порт открыт.
	if buildTime > 10*time.Second {
		t.Errorf("сборка индексов заняла %s — слишком долго для старта", buildTime)
	}

	// Задержка на реальном объёме. Отдельно замеряется ХУДШИЙ случай:
	// запрос, которого в справочнике нет, проходит подстрочный слой с
	// линейным проходом по 85 тыс. объектов.
	cases := []struct {
		name  string
		query string
	}{
		{"точное совпадение", snap.Street(0).Full()},
		{"префикс", "парков"},
		{"внутреннее слово", "адиля"},
		{"худший случай (нет в справочнике)", "щукинскаяяяю"},
	}

	for _, c := range cases {
		// Прогрев, затем медиана из 20 замеров: одиночный замер на
		// Windows слишком шумный.
		_ = snap.Suggest(index.Query{Text: c.query, Limit: 10})

		var worst time.Duration
		for range 20 {
			t0 := time.Now()
			_ = snap.Suggest(index.Query{Text: c.query, Limit: 10})
			if el := time.Since(t0); el > worst {
				worst = el
			}
		}
		t.Logf("%-36s худшее из 20: %s", c.name, worst.Round(time.Microsecond))

		if worst > 5*time.Millisecond {
			t.Errorf("%s: %s при бюджете 5 мс", c.name, worst.Round(time.Microsecond))
		}
	}
}

// TestFull_HouseLookup — проверка существования дома на реальных данных.
//
// Совпадение не должно быть стопроцентным: справочник ФНС не содержит части
// реальных строений (§8.7 описи), и именно поэтому HouseQC различает
// «нет в ГАР» и «не найден». Тест сторожит, что механизм работает и что
// доля найденных не обрушилась.
func TestFull_HouseLookup(t *testing.T) {
	d, hdr := loadFull(t)

	snap := index.Build(d.Streets, hdr.Version)
	snap.SetHouses(d.Houses)

	// Ключ улицы НЕ уникален: «Центральная улица» есть в сотнях населённых
	// пунктов области. Первая версия теста складывала их в map[string]uint32,
	// последняя запись затирала предыдущие, и московский адрес проверялся по
	// домам случайной деревни — отсюда 58.5% вместо реальной доли.
	// Проверять надо ВСЕ объекты с таким ключом.
	byKey := map[string][]uint32{}
	for i := 0; i < snap.Len(); i++ {
		k := normalize.StreetKey(snap.Street(uint32(i)).Full())
		byKey[k] = append(byKey[k], uint32(i))
	}

	rows := readCSVRows(t, "addresses.csv")
	var inGar, notInGar, noStreet int
	for _, r := range rows {
		if len(r) < 5 || r[3] == "" || r[4] == "" {
			continue
		}
		ids, ok := byKey[normalize.StreetKey(r[3])]
		if !ok {
			noStreet++
			continue
		}

		house := parse.ParseHouse(r[4])
		found := false
		for _, id := range ids {
			// Предпочитаем объект того же региона: адрес Москвы не должен
			// проверяться по домам одноимённой улицы в области.
			if r[1] != "" && snap.Street(id).Region != r[1] {
				continue
			}
			if snap.LookupHouse(id, house) == parse.HouseInGar {
				found = true
				break
			}
		}
		if found {
			inGar++
		} else {
			notInGar++
		}
	}

	total := inGar + notInGar
	if total == 0 {
		t.Skip("нечего проверять")
	}
	rate := float64(inGar) / float64(total)
	t.Logf("наших адресов с известной улицей %d", total)
	t.Logf("дом найден в ГАР: %d (%.1f%%)", inGar, rate*100)
	t.Logf("дома нет в ГАР:   %d (%.1f%%)", notInGar, 100-rate*100)
	t.Logf("улица не найдена: %d", noStreet)

	if rate < 0.60 {
		t.Errorf("дом найден лишь у %.1f%% адресов — вероятно, каноническая "+
			"форма из ГАР расходится с формой пользовательского ввода", rate*100)
	}
}

func readCSVRows(t *testing.T, name string) [][]string {
	t.Helper()

	path := filepath.Join("..", "..", "testdata", "golden", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("golden-корпус недоступен (%v)", err)
	}

	lines := splitLines(data)
	out := make([][]string, 0, len(lines))
	for i, l := range lines {
		if i == 0 {
			continue
		}
		out = append(out, splitCSV(l))
	}
	return out
}
