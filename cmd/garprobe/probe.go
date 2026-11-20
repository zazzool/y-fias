package main

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"time"

	"errors"

	"github.com/zazzool/y-fias/internal/garzip"
)

// Целевые структуры замера.
//
// Это НЕ окончательный формат снапшота — тот будет плоским, с пулом строк и
// mmap. Здесь взято заведомо расточительное представление на обычных
// Go-строках: если в память влезет оно, то оптимизированный снапшот влезет
// тем более. Мерить надо худший случай, иначе замер не доказывает ничего.

type addrObj struct {
	ObjectID int64
	Name     string // «Бакунинский»
	TypeName string // «пер»  — ГАР хранит тип ОТДЕЛЬНО от названия
	Level    uint8
}

type house struct {
	ObjectID int64
	Num      string // HOUSENUM
	Type     uint8  // 1 Владение · 2 Дом · 3 Домовладение · 4 Гараж
	AddNum1  string // корпус/строение/сооружение/литера
	AddType1 uint8
	AddNum2  string
	AddType2 uint8
}

type hierarchy struct {
	ObjectID int64
	ParentID int64
}

type probeResult struct {
	objs   []addrObj
	houses []house
	hier   []hierarchy

	byLevel   map[uint8]int
	houseNums map[string]int // распределение написаний номера дома
	skipped   int
}

func runProbe(zr *zip.Reader, want map[string]bool, ra *garzip.HTTPReaderAt) error {
	res := &probeResult{
		byLevel:   map[uint8]int{},
		houseNums: map[string]int{},
	}

	var memBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memBefore)

	start := time.Now()

	// Порядок обхода — от мелких файлов к крупным, чтобы ошибка в разборе
	// вылезла на первых секундах, а не через полчаса.
	order := []string{"AS_ADDR_OBJ", "AS_ADM_HIERARCHY", "AS_HOUSES"}
	for _, kind := range order {
		for _, f := range zr.File {
			e := classify(f.Name)
			if e.kind != kind || (e.region != "" && !want[e.region]) {
				continue
			}
			fmt.Printf("  %-18s регион %-3s %10s ... ",
				e.kind, e.region, humanBytes(int64(f.CompressedSize64)))

			t0 := time.Now()
			n, err := parseEntry(f, kind, res)
			if err != nil {
				fmt.Println()
				return fmt.Errorf("разбор %s: %w", f.Name, err)
			}
			fmt.Printf("%9d записей за %s\n", n, time.Since(t0).Round(time.Second))
		}
	}

	elapsed := time.Since(start)

	var memAfter runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&memAfter)

	report(res, &memBefore, &memAfter, elapsed, ra)
	return nil
}

// parseEntry потоково читает одну запись архива.
//
// Держать в памяти распакованный файл нельзя: AS_HOUSES по Московской области
// — 1.2 ГБ. Поэтому xml.Decoder читает из потока, а мы забираем только
// актуальные версии объектов.
func parseEntry(f *zip.File, kind string, res *probeResult) (int, error) {
	rc, err := f.Open()
	if err != nil {
		return 0, err
	}
	defer func() { _ = rc.Close() }()

	dec := xml.NewDecoder(rc)
	dec.Strict = false

	var count int
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, err
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		a := attrs(se)

		// ГАР хранит ВСЕ версии объекта: правки, переименования, упразднения.
		// Нам нужна одна — действующая. ISACTIVE=1 и ISACTUAL=1 вместе дают
		// «объект существует сейчас и это его текущая редакция».
		// Без этого фильтра объём вырастает примерно втрое, а в подсказках
		// появляются улицы, переименованные двадцать лет назад.
		if a["ISACTIVE"] != "1" || (a["ISACTUAL"] != "" && a["ISACTUAL"] != "1") {
			res.skipped++
			continue
		}

		switch kind {
		case "AS_ADDR_OBJ":
			lvl := uint8(atoiDefault(a["LEVEL"], 0))
			res.objs = append(res.objs, addrObj{
				ObjectID: atoi64(a["OBJECTID"]),
				Name:     a["NAME"],
				TypeName: a["TYPENAME"],
				Level:    lvl,
			})
			res.byLevel[lvl]++

		case "AS_HOUSES":
			h := house{
				ObjectID: atoi64(a["OBJECTID"]),
				Num:      a["HOUSENUM"],
				Type:     uint8(atoiDefault(a["HOUSETYPE"], 0)),
				AddNum1:  a["ADDNUM1"],
				AddType1: uint8(atoiDefault(a["ADDTYPE1"], 0)),
				AddNum2:  a["ADDNUM2"],
				AddType2: uint8(atoiDefault(a["ADDTYPE2"], 0)),
			}
			res.houses = append(res.houses, h)
			res.houseNums[renderHouse(h)]++

		case "AS_ADM_HIERARCHY":
			res.hier = append(res.hier, hierarchy{
				ObjectID: atoi64(a["OBJECTID"]),
				ParentID: atoi64(a["PARENTOBJID"]),
			})
		}
		count++
	}
	return count, nil
}

// renderHouse собирает «человеческое» написание номера, как его увидел бы
// диспетчер: 7к7, 10с4, вл15.
func renderHouse(h house) string {
	var b strings.Builder
	if h.Type == 1 {
		b.WriteString("вл")
	}
	b.WriteString(h.Num)
	for _, p := range []struct {
		num string
		typ uint8
	}{{h.AddNum1, h.AddType1}, {h.AddNum2, h.AddType2}} {
		if p.num == "" {
			continue
		}
		switch p.typ {
		case 1:
			b.WriteString("к")
		case 2:
			b.WriteString("с")
		case 3:
			b.WriteString("соор")
		case 4:
			b.WriteString("лит")
		}
		b.WriteString(p.num)
	}
	return b.String()
}

func report(res *probeResult, before, after *runtime.MemStats, elapsed time.Duration, ra *garzip.HTTPReaderAt) {
	used := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	if used < 0 {
		used = int64(after.HeapAlloc)
	}
	total := len(res.objs) + len(res.houses) + len(res.hier)

	fmt.Printf("\n%s\n", strings.Repeat("=", 66))
	fmt.Println("РЕЗУЛЬТАТ ЗАМЕРА")
	fmt.Println(strings.Repeat("=", 66))

	fmt.Printf("\nЗагружено (только действующие версии):\n")
	fmt.Printf("  адресных объектов   %10d\n", len(res.objs))
	fmt.Printf("  домов               %10d\n", len(res.houses))
	fmt.Printf("  связей иерархии     %10d\n", len(res.hier))
	fmt.Printf("  пропущено устаревших%10d\n", res.skipped)

	fmt.Printf("\nАдресные объекты по уровням ГАР:\n")
	levels := map[uint8]string{
		1: "субъект РФ", 2: "адм. район", 3: "мун. район", 4: "поселение",
		5: "город", 6: "нас. пункт", 7: "элемент планировки",
		8: "улица", 9: "земельный участок", 10: "здание", 17: "внутригор. территория",
	}
	keys := make([]int, 0, len(res.byLevel))
	for k := range res.byLevel {
		keys = append(keys, int(k))
	}
	sort.Ints(keys)
	for _, k := range keys {
		fmt.Printf("  %2d %-22s %8d\n", k, levels[uint8(k)], res.byLevel[uint8(k)])
	}

	fmt.Printf("\nПамять:\n")
	fmt.Printf("  занято структурами    %10s\n", humanBytes(used))
	fmt.Printf("  пик кучи (HeapSys)    %10s\n", humanBytes(int64(after.HeapSys)))
	if total > 0 {
		fmt.Printf("  на одну запись        %10.1f Б\n", float64(used)/float64(total))
	}

	fmt.Printf("\nСеть и время:\n")
	fetched, reqs := ra.Stats()
	fmt.Printf("  скачано               %10s за %d запросов\n", humanBytes(fetched), reqs)
	fmt.Printf("  весь архив            %10s\n", humanBytes(ra.Size()))
	fmt.Printf("  экономия              %9.1f%%\n", 100*(1-float64(fetched)/float64(ra.Size())))
	fmt.Printf("  время разбора         %10s\n", elapsed.Round(time.Second))

	fmt.Printf("\nНаписания номера дома: %d уникальных\n", len(res.houseNums))
	fmt.Printf("  (в нашей базе — 2 034 на 10 694 адреса)\n")

	fmt.Printf("\n%s\n", strings.Repeat("-", 66))
	const limitBytes = 1 << 30
	const limitTime = 30 * time.Minute
	okMem := used <= limitBytes
	okTime := elapsed <= limitTime
	fmt.Printf("КРИТЕРИЙ ВЫХОДА ЭТАПА 0 (docs/03-roadmap.md)\n")
	fmt.Printf("  рабочий набор <= 1 ГБ   %s (%s)\n", verdict(okMem), humanBytes(used))
	fmt.Printf("  сборка <= 30 минут      %s (%s)\n", verdict(okTime), elapsed.Round(time.Second))
	if okMem && okTime {
		fmt.Printf("\nГипотеза «справочник влезает в память» ПОДТВЕРЖДЕНА.\n")
	} else {
		fmt.Printf("\nГипотеза НЕ подтверждена. Пересмотреть §11 п.1 архитектуры\n")
		fmt.Printf("ДО начала этапа 1.\n")
	}
}

func verdict(ok bool) string {
	if ok {
		return "ДА "
	}
	return "НЕТ"
}

func attrs(se xml.StartElement) map[string]string {
	m := make(map[string]string, len(se.Attr))
	for _, a := range se.Attr {
		m[a.Name.Local] = a.Value
	}
	return m
}

func atoi64(s string) int64 {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	return int(atoi64(s))
}
