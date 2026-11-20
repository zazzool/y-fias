// Команда yfias-geocode собирает координаты адресов из OpenStreetMap.
//
// Зачем. Координат нет ни у одного из наших 10 694 адресов (§2.3 описи),
// поэтому район выводится эвристикой по соседним домам — а она заведомо врёт
// на 181 улице, пересекающей границы районов. С координатами район
// определяется попаданием в полигон, то есть точно.
//
// Запускается офлайн и редко: адресные точки OSM меняются медленно, а нагрузку
// на общий Overpass создавать незачем.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/zazzool/y-fias/internal/geo"
	"github.com/zazzool/y-fias/internal/normalize"
	"github.com/zazzool/y-fias/internal/parse"
	"github.com/zazzool/y-fias/internal/snapshot"
)

func main() {
	var (
		in = flag.String("in", "data/osm/moscow_addr.json,data/osm/addr50.jsonl",
			"файлы с адресными точками через запятую: ответ Overpass (.json) или порайонная выгрузка (.jsonl)")
		out      = flag.String("out", "data/coords.yfcoord", "куда положить координаты")
		geoPath  = flag.String("geo", "data/geo.json", "границы районов")
		snapPath = flag.String("snapshot", "data/gar.yfsnap", "справочник для сверки покрытия")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	if err := run(log, *in, *out, *geoPath, *snapPath); err != nil {
		log.Error("геокодирование не удалось", "ошибка", err)
		os.Exit(1)
	}
}

// overpassElement — элемент ответа Overpass.
type overpassElement struct {
	Type   string  `json:"type"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	Center *struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"center"`
	Tags map[string]string `json:"tags"`
}

func run(log *slog.Logger, in, out, geoPath, snapPath string) error {
	start := time.Now()

	// Источников может быть несколько: Москва выгружается одним запросом, а
	// область — по районам, потому что целиком она не помещается в ответ
	// публичного Overpass.
	var points []rawPoint
	for _, path := range strings.Split(in, ",") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		got, err := readPoints(path)
		if err != nil {
			// Отсутствие одного источника не должно ронять сборку: область
			// может быть ещё не выгружена.
			log.Warn("источник пропущен", "файл", path, "причина", err)
			continue
		}
		log.Info("источник прочитан", "файл", path, "точек", len(got))
		points = append(points, got...)
	}
	if len(points) == 0 {
		return fmt.Errorf("не прочитано ни одной адресной точки")
	}
	log.Info("адресные точки прочитаны", "всего", len(points))

	// Район точки определяется её же координатой. Иначе ключ адреса не
	// уникален: «Центральная улица, 1» есть в сотнях населённых пунктов, и
	// без района каждый такой дом получил бы чужую точку.
	areas, err := loadAreas(geoPath)
	if err != nil {
		return fmt.Errorf("границы районов обязательны для геокодирования: %w", err)
	}
	log.Info("границы районов загружены", "областей", areas.Len())

	coords := geo.NewCoords()

	var (
		noCoord   int
		noParse   int
		noArea    int
		streets   = map[string]int{}
		houseAmb  int
		districts = map[string]int{}
	)

	for _, pt := range points {
		lat, lon := pt.Lat, pt.Lon
		if lat == 0 && lon == 0 {
			noCoord++
			continue
		}

		street, house := pt.Street, pt.House
		if street == "" || house == "" {
			noParse++
			continue
		}

		// Ключи считаются НАШИМИ правилами, а не как есть: иначе «ул.
		// Саляма Адиля» из OSM и «Саляма Адиля улица» из ГАР не совпадут,
		// и координаты пропадут впустую.
		key := normalize.StreetKey(street)
		canon := parse.ParseHouse(house).Canonical()
		if key == "" || canon == "" {
			noParse++
			continue
		}

		area, _ := areas.Locate(geo.Point{Lon: lon, Lat: lat})
		if area == nil {
			// Точка вне известных границ: определить, к какому району она
			// относится, нечем, а класть её с пустым районом значило бы
			// вернуть ту же неоднозначность.
			noArea++
			continue
		}

		// Один и тот же адрес встречается в OSM несколько раз (здание и
		// точка внутри него). Берём первый: расхождение между ними —
		// десятки метров, а район от этого не меняется.
		if _, exists := coords.Lookup(area.Name, key, canon); exists {
			houseAmb++
			continue
		}

		coords.Put(area.Name, key, canon, geo.Coord{Lon: lon, Lat: lat})
		streets[key]++
		districts[area.Name]++
	}

	log.Info("координаты собраны",
		"адресов", coords.Len(),
		"улиц", len(streets),
		"районов", len(districts),
		"вне границ", noArea,
		"без координат", noCoord,
		"без разбора", noParse,
		"повторов", houseAmb)

	if coords.Len() == 0 {
		return fmt.Errorf("не собрано ни одной координаты")
	}

	if err := geo.WriteCoords(out, coords); err != nil {
		return fmt.Errorf("запись %s: %w", out, err)
	}
	st, _ := os.Stat(out)
	log.Info("координаты записаны", "файл", out,
		"размер", fmt.Sprintf("%.2f МБ", float64(st.Size())/(1<<20)))

	// Проверочное чтение: файл, который не читается обратно, бесполезен.
	back, err := geo.ReadCoords(out)
	if err != nil {
		return fmt.Errorf("проверочное чтение: %w", err)
	}
	if back.Len() != coords.Len() {
		return fmt.Errorf("проверочное чтение: %d координат вместо %d",
			back.Len(), coords.Len())
	}

	if snapPath != "" {
		reportCoverage(log, snapPath, back)
		reportOurCoverage(log, back, regionOfDistrict(areas))
	}

	log.Info("готово", "за", time.Since(start).Round(time.Second).String())
	return nil
}

// rawPoint — адресная точка независимо от формата источника.
type rawPoint struct {
	Lon, Lat      float64
	Street, House string
}

// readPoints читает точки из ответа Overpass (.json) или из порайонной
// выгрузки (.jsonl).
func readPoints(path string) ([]rawPoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	if strings.HasSuffix(path, ".jsonl") {
		return readJSONL(f)
	}

	var resp struct {
		Elements []overpassElement `json:"elements"`
	}
	if err := json.NewDecoder(f).Decode(&resp); err != nil {
		return nil, fmt.Errorf("разбор: %w", err)
	}

	out := make([]rawPoint, 0, len(resp.Elements))
	for _, el := range resp.Elements {
		lat, lon := el.Lat, el.Lon
		if el.Center != nil {
			lat, lon = el.Center.Lat, el.Center.Lon
		}
		out = append(out, rawPoint{
			Lon: lon, Lat: lat,
			Street: el.Tags["addr:street"],
			House:  el.Tags["addr:housenumber"],
		})
	}
	return out, nil
}

func readJSONL(f *os.File) ([]rawPoint, error) {
	sc := bufio.NewScanner(f)
	// Строка — целый район, она может быть в десятки мегабайт.
	sc.Buffer(make([]byte, 1<<20), 256<<20)

	var out []rawPoint
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			Points []struct {
				Lon    float64 `json:"lon"`
				Lat    float64 `json:"lat"`
				Street string  `json:"street"`
				House  string  `json:"house"`
			} `json:"points"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// Оборванная строка от прерванной выгрузки: пропускаем, район
			// доберётся повторным запуском.
			continue
		}
		for _, p := range rec.Points {
			out = append(out, rawPoint{Lon: p.Lon, Lat: p.Lat, Street: p.Street, House: p.House})
		}
	}
	return out, sc.Err()
}

// reportCoverage показывает, для скольких домов справочника нашлись координаты.
//
// Покрытие заведомо неполное: OSM размечен людьми и по Москве неравномерен.
// Значение имеет не сам уровень, а то, что он известен — район по геометрии
// определяется только там, где координата есть, а в остальных случаях
// работает прежний вывод по соседям.
func reportCoverage(log *slog.Logger, snapPath string, c *geo.Coords) {
	data, _, err := snapshot.Read(snapPath)
	if err != nil {
		log.Warn("справочник недоступен, покрытие не посчитано", "причина", err)
		return
	}

	var withCoord, total int
	byStreet := map[string]bool{}

	for pos, list := range data.Houses {
		if int(pos) >= len(data.Streets) {
			continue
		}
		key := data.Streets[pos].Key
		if key == "" {
			key = normalize.StreetKey(data.Streets[pos].Full())
		}
		district := data.Streets[pos].District
		for _, h := range list {
			total++
			if _, ok := c.Lookup(district, key, h.Canonical); ok {
				withCoord++
				byStreet[key] = true
			}
		}
	}

	if total == 0 {
		log.Info("в справочнике нет домов, покрытие не считалось")
		return
	}
	log.Info("покрытие справочника координатами",
		"домов", total,
		"с координатой", withCoord,
		"доля", fmt.Sprintf("%.1f%%", 100*float64(withCoord)/float64(total)),
		"улиц с координатами", len(byStreet))
}

// reportOurCoverage — главное число: для скольких НАШИХ адресов нашлась
// координата. Покрытие справочника ГАР интересно куда меньше: в нём вся
// область, а вызовы идут по нашим 10 694 адресам.
// regionOfDistrict — к какому региону относится район.
//
// Нужна, чтобы отличить настоящее расхождение от совпадения названий между
// регионами: «Дмитровский проезд, 18» есть и в Москве, и в Талдомском
// городском округе, и это не ошибка наших данных.
func regionOfDistrict(ix *geo.Index) map[string]string {
	out := map[string]string{}
	for _, a := range ix.Areas() {
		out[normDistrictName(a.Name)] = a.Region
	}
	return out
}

func normDistrictName(s string) string {
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

func reportOurCoverage(log *slog.Logger, c *geo.Coords, regionOf map[string]string) {
	rows, err := readOurAddresses()
	if err != nil {
		log.Warn("golden-корпус недоступен, покрытие наших адресов не посчитано",
			"причина", err)
		return
	}

	var (
		withCoord     int
		wrongDistrict int
		otherRegion   int
		notFound      int
		examples      []string
	)
	for _, r := range rows {
		key := normalize.StreetKey(r.street)
		canon := parse.ParseHouse(r.house).Canonical()
		if key == "" || canon == "" {
			continue
		}
		if _, ok := c.Lookup(r.district, key, canon); ok {
			withCoord++
			continue
		}

		// Адрес не нашёлся в записанном районе. Возможны три случая:
		// точки нет вовсе; она есть в другом районе ТОГО ЖЕ региона — тогда
		// записанный у нас район, скорее всего, неверен; она есть в другом
		// РЕГИОНЕ — а это просто совпадение названий, и находкой не является.
		elsewhere := c.LookupAnyDistrict(key, canon)
		if len(elsewhere) == 0 {
			notFound++
			continue
		}

		var sameRegion string
		for d := range elsewhere {
			if regionOf[normDistrictName(d)] == r.region {
				sameRegion = d
				break
			}
		}
		if sameRegion == "" {
			otherRegion++
			continue
		}

		wrongDistrict++
		if len(examples) < 12 {
			examples = append(examples,
				fmt.Sprintf("%s, %s: у нас %q, по координате %q",
					r.street, r.house, r.district, sameRegion))
		}
	}

	log.Info("ПОКРЫТИЕ НАШИХ АДРЕСОВ",
		"всего", len(rows),
		"с координатой", withCoord,
		"доля", fmt.Sprintf("%.1f%%", 100*float64(withCoord)/float64(len(rows))),
		"район расходится", wrongDistrict,
		"совпадение имён в другом регионе", otherRegion,
		"точки нет", notFound)

	for _, e := range examples {
		log.Info("расхождение района", "адрес", e)
	}
}

type ourAddress struct{ street, house, district, region string }

func readOurAddresses() ([]ourAddress, error) {
	data, err := os.ReadFile("testdata/golden/addresses.csv")
	if err != nil {
		return nil, err
	}

	var out []ourAddress
	lines := strings.Split(strings.ReplaceAll(string(data), "\r", ""), "\n")
	for i, l := range lines {
		if i == 0 || l == "" {
			continue
		}
		f := splitCSV(l)
		// id,region,district,street,house,...
		if len(f) < 5 || f[3] == "" {
			continue
		}
		out = append(out, ourAddress{
			street: f[3], house: f[4], district: f[2], region: f[1],
		})
	}
	return out, nil
}

func splitCSV(line string) []string {
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

func loadAreas(path string) (*geo.Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var areas []geo.Area
	if err := json.NewDecoder(f).Decode(&areas); err != nil {
		return nil, err
	}
	return geo.NewIndex(areas), nil
}

// топ улиц по числу точек — для беглой проверки, что сопоставление вообще
// работает, а не выдаёт правдоподобный ноль.
func topStreets(m map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	all := make([]kv, 0, len(m))
	for k, v := range m {
		all = append(all, kv{k, v})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v > all[j].v })

	out := make([]string, 0, n)
	for i, e := range all {
		if i >= n {
			break
		}
		out = append(out, fmt.Sprintf("%s (%d)", e.k, e.v))
	}
	return out
}

var _ = topStreets
var _ = strings.TrimSpace
