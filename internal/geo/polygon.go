// Package geo отвечает на вопрос «в каком районе находится точка».
//
// Зачем это нужно. Район определяет, какой ПНД обслуживает адрес. Сегодня он
// выводится по соседним домам, и на 181 улице, пересекающей границы районов
// (§2.3 описи), такой вывод уверенно врёт. Геометрия отвечает точно — там, где
// у адреса есть координаты.
//
// Координат у наших адресов пока нет ни у одного (§2.3), поэтому полигоны —
// это готовый механизм, ждущий источник координат, а не работающая сегодня
// функция. Порядок именно такой: сначала точный метод, потом данные для него.
package geo

import "math"

// Point — географическая точка. Долгота и широта в градусах.
type Point struct {
	Lon float64
	Lat float64
}

// Ring — замкнутый контур.
type Ring []Point

// Polygon — область с дырами.
type Polygon struct {
	Outer Ring
	Inner []Ring
}

// Area — именованная область: район, поселение, округ.
type Area struct {
	ID       int64  // идентификатор отношения OSM
	Name     string // очищенное название: «Басманный»
	RawName  string // как в OSM: «Басманный район»
	Level    int    // admin_level
	Region   string // «77», «50»
	Polygons []Polygon

	// Ограничивающий прямоугольник — предфильтр.
	MinLon, MinLat, MaxLon, MaxLat float64
}

// bbox пересчитывает ограничивающий прямоугольник.
func (a *Area) bbox() {
	a.MinLon, a.MinLat = math.Inf(1), math.Inf(1)
	a.MaxLon, a.MaxLat = math.Inf(-1), math.Inf(-1)

	for _, p := range a.Polygons {
		for _, pt := range p.Outer {
			a.MinLon = math.Min(a.MinLon, pt.Lon)
			a.MinLat = math.Min(a.MinLat, pt.Lat)
			a.MaxLon = math.Max(a.MaxLon, pt.Lon)
			a.MaxLat = math.Max(a.MaxLat, pt.Lat)
		}
	}
}

// containsBBox — быстрая отбраковка по прямоугольнику.
func (a *Area) containsBBox(pt Point) bool {
	return pt.Lon >= a.MinLon && pt.Lon <= a.MaxLon &&
		pt.Lat >= a.MinLat && pt.Lat <= a.MaxLat
}

// Contains проверяет попадание точки в область с учётом дыр.
func (a *Area) Contains(pt Point) bool {
	if !a.containsBBox(pt) {
		return false
	}
	for _, poly := range a.Polygons {
		if !ringContains(poly.Outer, pt) {
			continue
		}
		inHole := false
		for _, hole := range poly.Inner {
			if ringContains(hole, pt) {
				inHole = true
				break
			}
		}
		if !inHole {
			return true
		}
	}
	return false
}

// ringContains — метод трассировки луча (чётность пересечений).
//
// Границы районов не самопересекаются, поэтому чётно-нечётного правила
// достаточно и более дорогой winding number не нужен.
func ringContains(r Ring, pt Point) bool {
	if len(r) < 3 {
		return false
	}

	inside := false
	j := len(r) - 1
	for i := range r {
		pi, pj := r[i], r[j]
		// Луч идёт вправо от точки. Ребро учитывается, если точка лежит
		// между широтами его концов; строгое сравнение с одной стороны
		// исключает двойной счёт вершин.
		if (pi.Lat > pt.Lat) != (pj.Lat > pt.Lat) {
			x := (pj.Lon-pi.Lon)*(pt.Lat-pi.Lat)/(pj.Lat-pi.Lat) + pi.Lon
			if pt.Lon < x {
				inside = !inside
			}
		}
		j = i
	}
	return inside
}

/* ── Индекс областей ───────────────────────────────────────────── */

// Index — набор областей с поиском по точке.
//
// Сеточный индекс сужает круг кандидатов, затем идёт точная проверка
// трассировкой луча. Обоснование сетки — в grid.go: замер показал, что одного
// предфильтра по прямоугольнику мало.
type Index struct {
	areas []Area
	grid  *grid
}

func NewIndex(areas []Area) *Index {
	for i := range areas {
		areas[i].bbox()
	}
	return &Index{areas: areas, grid: buildGrid(areas)}
}

func (ix *Index) Len() int { return len(ix.areas) }

// Areas — все области.
func (ix *Index) Areas() []Area { return ix.areas }

// Locate возвращает область, содержащую точку.
//
// Второе значение — сколько областей подошло. Больше одной означает
// пересекающиеся границы в данных OSM: это повод для ревью, а не для
// молчаливого выбора первой попавшейся.
func (ix *Index) Locate(pt Point) (*Area, int) {
	var (
		found *Area
		n     int
	)
	for _, i := range ix.grid.candidates(pt) {
		if ix.areas[i].Contains(pt) {
			n++
			if found == nil {
				found = &ix.areas[i]
			}
		}
	}
	return found, n
}
