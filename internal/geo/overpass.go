package geo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OverpassURL — публичный экземпляр Overpass API.
const OverpassURL = "https://overpass-api.de/api/interpreter"

// Идентификаторы областей Overpass: 3600000000 + номер отношения OSM.
const (
	AreaMoscow = 3600102269 // Москва
	AreaMO     = 3600051490 // Московская область
)

// Fetch запрашивает границы административного уровня внутри области.
//
// Overpass — общий публичный сервис с квотой в два слота, и отказ 429 при
// обычной работе не исключение, а норма. Поэтому повторы встроены сюда:
// вызывающий код не должен знать про чужие лимиты. По той же причине
// геометрия НЕ используется в рантайме — она загружается офлайн и кладётся в
// файл (§11.5 архитектуры: смена границы района это смена зоны
// ответственности ПНД, она не должна происходить незаметно).
func Fetch(ctx context.Context, client *http.Client, areaID int64, levels []int, region string) ([]Area, error) {
	query := buildQuery(areaID, levels)

	body, err := post(ctx, client, query)
	if err != nil {
		return nil, err
	}

	var resp overpassResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("разбор ответа Overpass: %w", err)
	}

	areas := make([]Area, 0, len(resp.Elements))
	for _, el := range resp.Elements {
		if el.Type != "relation" {
			continue
		}
		a := Area{
			ID:      el.ID,
			RawName: el.Tags["name"],
			Name:    CleanAreaName(el.Tags["name"]),
			Region:  region,
		}
		a.Level, _ = strconv.Atoi(el.Tags["admin_level"])

		if poly := assemble(el.Members); len(poly.Outer) >= 3 {
			a.Polygons = append(a.Polygons, poly)
		}
		if len(a.Polygons) == 0 {
			continue
		}
		areas = append(areas, a)
	}
	return areas, nil
}

func buildQuery(areaID int64, levels []int) string {
	var b strings.Builder
	b.WriteString("[out:json][timeout:600];\n(\n")
	for _, l := range levels {
		fmt.Fprintf(&b, "  relation[\"boundary\"=\"administrative\"][\"admin_level\"=\"%d\"](area:%d);\n",
			l, areaID)
	}
	b.WriteString(");\nout geom;\n")
	return b.String()
}

// PostOverpass выполняет произвольный запрос к Overpass с повторами.
//
// Экспортируется, потому что адресные точки выгружаются отдельной командой:
// логика повторов у них общая, а дублировать её значило бы получить две
// разные политики отношения к чужой квоте.
func PostOverpass(ctx context.Context, client *http.Client, query string) ([]byte, error) {
	return post(ctx, client, query)
}

// post выполняет запрос с повторами при 429 и 504.
func post(ctx context.Context, client *http.Client, query string) ([]byte, error) {
	const attempts = 6

	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			// Отступ растёт: квота Overpass освобождается за десятки секунд.
			delay := time.Duration(attempt*attempt) * 15 * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, OverpassURL,
			bytes.NewReader([]byte(query)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		// Overpass просит представляться: анонимные клиенты режутся первыми.
		req.Header.Set("User-Agent", "y-fias/1.0 (address service for emergency dispatch)")

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		switch resp.StatusCode {
		case http.StatusOK:
			if readErr != nil {
				return nil, readErr
			}
			return body, nil
		case http.StatusTooManyRequests, http.StatusGatewayTimeout,
			http.StatusServiceUnavailable:
			lastErr = fmt.Errorf("Overpass ответил %d (квота или перегрузка)", resp.StatusCode)
		default:
			return nil, fmt.Errorf("Overpass ответил %d: %s",
				resp.StatusCode, firstLine(body))
		}
	}
	return nil, fmt.Errorf("Overpass недоступен после %d попыток: %w", attempts, lastErr)
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

/* ── Разбор ответа ─────────────────────────────────────────────── */

type overpassResponse struct {
	Elements []struct {
		Type    string            `json:"type"`
		ID      int64             `json:"id"`
		Tags    map[string]string `json:"tags"`
		Members []member          `json:"members"`
	} `json:"elements"`
}

type member struct {
	Type     string `json:"type"`
	Ref      int64  `json:"ref"`
	Role     string `json:"role"`
	Geometry []struct {
		Lat float64 `json:"lat"`
		Lon float64 `json:"lon"`
	} `json:"geometry"`
}

// assemble собирает кольца из кусков границы.
//
// В OSM граница района — это отношение из отрезков-линий, идущих в
// произвольном порядке и произвольном направлении. Просто склеить их подряд
// нельзя: получится ломаная, которая сама себя пересекает, и проверка
// попадания точки начнёт врать. Куски надо сшивать по совпадающим концам,
// переворачивая при необходимости.
func assemble(members []member) Polygon {
	var poly Polygon

	outer := collectSegments(members, "outer", "")
	inner := collectSegments(members, "inner", "enclave")

	rings := stitch(outer)
	if len(rings) == 0 {
		return poly
	}

	// Внешним кольцом считается самое длинное: у района это его граница,
	// остальные — анклавы и артефакты разметки.
	best := 0
	for i, r := range rings {
		if len(r) > len(rings[best]) {
			best = i
		}
		_ = i
	}
	poly.Outer = rings[best]
	for i, r := range rings {
		if i != best && len(r) >= 3 {
			poly.Inner = append(poly.Inner, r)
		}
	}
	poly.Inner = append(poly.Inner, stitch(inner)...)
	return poly
}

func collectSegments(members []member, roles ...string) [][]Point {
	want := map[string]bool{}
	for _, r := range roles {
		want[r] = true
	}

	var segs [][]Point
	for _, m := range members {
		if m.Type != "way" || !want[m.Role] || len(m.Geometry) < 2 {
			continue
		}
		seg := make([]Point, 0, len(m.Geometry))
		for _, g := range m.Geometry {
			seg = append(seg, Point{Lon: g.Lon, Lat: g.Lat})
		}
		segs = append(segs, seg)
	}
	return segs
}

// stitch сшивает отрезки в замкнутые кольца.
func stitch(segs [][]Point) []Ring {
	used := make([]bool, len(segs))
	var rings []Ring

	for i := range segs {
		if used[i] {
			continue
		}
		used[i] = true
		cur := append(Ring{}, segs[i]...)

		// Наращиваем кольцо, пока находится отрезок с общим концом.
		for {
			grew := false
			for j := range segs {
				if used[j] {
					continue
				}
				s := segs[j]
				switch {
				case samePoint(cur[len(cur)-1], s[0]):
					cur = append(cur, s[1:]...)
				case samePoint(cur[len(cur)-1], s[len(s)-1]):
					cur = append(cur, reverse(s)[1:]...)
				case samePoint(cur[0], s[len(s)-1]):
					cur = append(append(Ring{}, s[:len(s)-1]...), cur...)
				case samePoint(cur[0], s[0]):
					cur = append(append(Ring{}, reverse(s)[:len(s)-1]...), cur...)
				default:
					continue
				}
				used[j] = true
				grew = true
			}
			if !grew || samePoint(cur[0], cur[len(cur)-1]) {
				break
			}
		}

		if len(cur) >= 3 {
			rings = append(rings, cur)
		}
	}
	return rings
}

// samePoint сравнивает координаты с допуском.
//
// Точный порядок float здесь не годится: одна и та же точка приходит в разных
// отрезках, и после сериализации в JSON её представление может отличаться в
// последнем разряде. Допуск в 1e-9 градуса — это доли миллиметра.
func samePoint(a, b Point) bool {
	const eps = 1e-9
	return abs(a.Lon-b.Lon) < eps && abs(a.Lat-b.Lat) < eps
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func reverse(p []Point) []Point {
	out := make([]Point, len(p))
	for i, v := range p {
		out[len(p)-1-i] = v
	}
	return out
}

// CleanAreaName снимает служебные слова из названия района OSM.
//
// OSM пишет «район Кунцево», «Мещанский район», «поселение Внуковское», а наша
// база и справочник ПНД — «Кунцево», «Мещанский», «Внуковское». Связь
// «район → ПНД» идёт по тексту, поэтому без очистки она не установится.
// Та же задача уже решалась для названий из ГАР, но словари разные: ГАР пишет
// тип впереди, OSM — с обеих сторон.
func CleanAreaName(name string) string {
	s := strings.TrimSpace(name)
	low := strings.ToLower(s)

	prefixes := []string{
		"муниципальный округ ", "городской округ ", "внутригородская территория ",
		"городское поселение ", "сельское поселение ", "поселение ", "район ", "округ ",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(low, p) {
			return strings.TrimSpace(s[len(p):])
		}
	}

	suffixes := []string{" район", " округ", " поселение"}
	for _, suf := range suffixes {
		if strings.HasSuffix(low, suf) {
			return strings.TrimSpace(s[:len(s)-len(suf)])
		}
	}
	return s
}
