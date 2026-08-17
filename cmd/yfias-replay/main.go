// Команда yfias-replay прогоняет golden-корпус через индекс и измеряет
// качество подсказок.
//
// Веса ранжирования подбираются здесь, а не подкручиваются в коде по
// ощущениям: §5.3 архитектуры требует, чтобы они были значением с версией,
// подобранным на корпусе.
package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
	"github.com/zazzool/y-fias/internal/snapshot"
)

func main() {
	var (
		golden = flag.String("golden", "testdata/golden", "каталог golden-корпуса")
		snap   = flag.String("snapshot", "", "снапшот ГАР; по умолчанию индекс строится из наших адресов")
		limit  = flag.Int("limit", 5, "сколько подсказок считать попаданием")
		tune   = flag.Bool("tune", false, "перебрать веса по сетке")
	)
	flag.Parse()

	if err := run(*golden, *snap, *limit, *tune); err != nil {
		fmt.Fprintf(os.Stderr, "ОШИБКА: %v\n", err)
		os.Exit(1)
	}
}

func run(goldenDir, snapPath string, limit int, tune bool) error {
	streets, err := loadStreets(goldenDir, snapPath)
	if err != nil {
		return err
	}

	start := time.Now()
	idx := index.Build(streets, 1)
	fmt.Printf("Индекс: %d объектов, собран за %s\n\n",
		idx.Len(), time.Since(start).Round(time.Millisecond))

	if tune {
		return tuneWeights(idx, limit)
	}

	m := measure(idx, index.DefaultWeights(), limit)
	m.print("текущие веса")
	return nil
}

/* ── Загрузка ──────────────────────────────────────────────────── */

// loadStreets собирает список объектов для индекса.
//
// Частоты вызовов берутся из наших данных ВСЕГДА, даже когда справочник
// пришёл из ГАР: это наш сигнал ранжирования, и без него подсказки теряют
// главное преимущество.
func loadStreets(goldenDir, snapPath string) ([]index.Street, error) {
	calls, err := loadCalls(goldenDir)
	if err != nil {
		return nil, err
	}

	if snapPath != "" {
		data, hdr, err := snapshot.Read(snapPath)
		if err != nil {
			return nil, err
		}
		streets := data.Streets
		fmt.Printf("Снапшот ГАР: версия %d, %d объектов, %d домов\n",
			hdr.Version, len(streets), hdr.Houses)

		var matched int
		for i := range streets {
			if n, ok := calls[normalize.StreetKey(streets[i].Full())]; ok {
				streets[i].Calls = n
				matched++
			}
		}
		fmt.Printf("Частоты вызовов подставлены для %d объектов\n", matched)
		return streets, nil
	}

	return loadOurStreets(goldenDir, calls)
}

// loadCalls считает вызовы по ключу улицы.
func loadCalls(dir string) (map[string]uint32, error) {
	addrRows, err := readCSV(filepath.Join(dir, "addresses.csv"))
	if err != nil {
		return nil, err
	}
	freqRows, err := readCSV(filepath.Join(dir, "freq.csv"))
	if err != nil {
		return nil, err
	}

	byAddr := make(map[string]uint32, len(freqRows))
	for _, r := range freqRows {
		if len(r) < 2 {
			continue
		}
		n, _ := strconv.Atoi(r[1])
		byAddr[r[0]] = uint32(n)
	}

	out := make(map[string]uint32, len(addrRows))
	for _, r := range addrRows {
		if len(r) < 4 || r[3] == "" {
			continue
		}
		out[normalize.StreetKey(r[3])] += byAddr[r[0]]
	}
	return out, nil
}

func loadOurStreets(dir string, calls map[string]uint32) ([]index.Street, error) {
	rows, err := readCSV(filepath.Join(dir, "addresses.csv"))
	if err != nil {
		return nil, err
	}

	seen := map[string]int{}
	var out []index.Street
	for _, r := range rows {
		if len(r) < 4 || r[3] == "" {
			continue
		}
		key := normalize.StreetKey(r[3])
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = len(out)

		name, typ := normalize.SplitStreetType(r[3])
		out = append(out, index.Street{
			Name:     name,
			Type:     typ,
			Key:      key,
			Region:   r[1],
			District: r[2],
			Calls:    calls[key],
		})
	}
	return out, nil
}

func readCSV(path string) ([][]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("чтение %s: %w", path, err)
	}
	if len(rows) < 2 {
		return nil, fmt.Errorf("%s пуст", path)
	}
	return rows[1:], nil
}

/* ── Измерение ─────────────────────────────────────────────────── */

type metrics struct {
	Streets  int
	Uniform  float64 // букв до подсказки, каждая улица весит одинаково
	Weighted float64 // то же, но улица весит по числу вызовов
	Never    int
	Latency  time.Duration
}

func (m metrics) print(label string) {
	fmt.Printf("%-28s улиц %d · равномерно %.3f · по вызовам %.3f · не найдено %d · медиана %s\n",
		label, m.Streets, m.Uniform, m.Weighted, m.Never, m.Latency.Round(time.Microsecond))
}

// measure считает, сколько букв надо ввести, чтобы улица попала в выдачу.
func measure(idx *index.Snapshot, w index.Weights, limit int) metrics {
	const maxLetters = 20

	var (
		m         metrics
		sum       int
		weightSum float64
		weighted  float64
		lat       []time.Duration
	)

	for i := 0; i < idx.Len(); i++ {
		st := idx.Street(uint32(i))
		runes := []rune(normalize.SearchNorm(st.Name))
		if len(runes) < 3 {
			continue
		}
		m.Streets++

		found := -1
		for n := 1; n <= min(len(runes), maxLetters); n++ {
			t0 := time.Now()
			res := idx.Suggest(index.Query{
				Text: string(runes[:n]), Limit: limit, Weights: w,
			})
			if len(lat) < 20000 {
				lat = append(lat, time.Since(t0))
			}
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
			m.Never++
			continue
		}
		sum += found

		weight := float64(max(st.Calls, 1))
		weightSum += weight
		weighted += weight * float64(found)
	}

	if ok := m.Streets - m.Never; ok > 0 {
		m.Uniform = float64(sum) / float64(ok)
	}
	if weightSum > 0 {
		m.Weighted = weighted / weightSum
	}
	if len(lat) > 0 {
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		m.Latency = lat[len(lat)/2]
	}
	return m
}

/* ── Подбор весов ──────────────────────────────────────────────── */

// tuneWeights перебирает сетку и печатает лучшие сочетания.
//
// Оптимизируется ВЗВЕШЕННАЯ метрика: равномерная считает одинаково значимыми
// улицу с 580 вызовами и улицу с одним, а диспетчер вводит первую
// несоизмеримо чаще.
func tuneWeights(idx *index.Snapshot, limit int) error {
	grid := []struct {
		name   string
		values []float64
		set    func(*index.Weights, float64)
	}{
		{"Calls", []float64{0, 3, 6, 10, 16}, func(w *index.Weights, v float64) { w.Calls = v }},
		{"Coverage", []float64{0, 5, 10, 20}, func(w *index.Weights, v float64) { w.Coverage = v }},
		{"ShortName", []float64{0, 4, 8}, func(w *index.Weights, v float64) { w.ShortName = v }},
	}

	base := index.DefaultWeights()
	fmt.Println("Базовые веса:")
	measure(idx, base, limit).print("  по умолчанию")
	fmt.Println()

	type result struct {
		w index.Weights
		m metrics
	}
	var all []result

	// Покоординатный перебор: полная сетка из 60 сочетаний на 8 тысячах улиц
	// заняла бы часы, а выигрыш от совместного перебора здесь невелик.
	cur := base
	for _, dim := range grid {
		best := cur
		bestM := measure(idx, cur, limit)

		for _, v := range dim.values {
			cand := cur
			dim.set(&cand, v)
			m := measure(idx, cand, limit)
			all = append(all, result{cand, m})

			fmt.Printf("  %-10s = %5.1f -> ", dim.name, v)
			m.print("")

			if m.Weighted < bestM.Weighted {
				best, bestM = cand, m
			}
		}
		cur = best
		fmt.Printf("  лучшее по %s: %.1f\n\n", dim.name, valueOf(dim.name, cur))
	}

	fmt.Println("ИТОГ")
	measure(idx, cur, limit).print("  подобранные веса")
	fmt.Printf("\n  Calls=%.1f Coverage=%.1f ShortName=%.1f Region=%.1f Patient=%.1f\n",
		cur.Calls, cur.Coverage, cur.ShortName, cur.Region, cur.Patient)
	fmt.Println("\nЕсли результат лучше текущего, перенесите значения в")
	fmt.Println("index.DefaultWeights() и поднимите Version.")
	return nil
}

func valueOf(dim string, w index.Weights) float64 {
	switch strings.ToLower(dim) {
	case "calls":
		return w.Calls
	case "coverage":
		return w.Coverage
	case "shortname":
		return w.ShortName
	}
	return 0
}
