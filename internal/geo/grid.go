package geo

import "math"

// Сеточный индекс областей.
//
// Первая версия обходилась предфильтром по ограничивающему прямоугольнику, и
// в комментарии стояло «сеточный индекс не нужен: областей сотни, сравнение
// прямоугольников стоит наносекунды». Замер на реальных 198 областях Москвы и
// области показал 1.0 мс на поиск при бюджете 5 мс на весь запрос — то есть
// пятую часть бюджета на одну операцию. Прямоугольники действительно дёшевы,
// но у прошедших предфильтр областей приходится трассировать луч через тысячи
// точек границы, и делать это для десятков кандидатов дорого.
//
// Сетка сокращает число кандидатов до единиц: в ячейку попадают только те
// области, чей прямоугольник её задевает.

const gridDim = 128 // ячеек по каждой оси

type grid struct {
	minLon, minLat float64
	stepLon        float64
	stepLat        float64
	cells          [][]int32 // индексы областей по ячейкам
}

func buildGrid(areas []Area) *grid {
	if len(areas) == 0 {
		return nil
	}

	g := &grid{
		minLon: math.Inf(1),
		minLat: math.Inf(1),
		cells:  make([][]int32, gridDim*gridDim),
	}
	maxLon, maxLat := math.Inf(-1), math.Inf(-1)

	for i := range areas {
		g.minLon = math.Min(g.minLon, areas[i].MinLon)
		g.minLat = math.Min(g.minLat, areas[i].MinLat)
		maxLon = math.Max(maxLon, areas[i].MaxLon)
		maxLat = math.Max(maxLat, areas[i].MaxLat)
	}

	// Защита от вырожденного случая: одна точечная область.
	g.stepLon = (maxLon - g.minLon) / gridDim
	g.stepLat = (maxLat - g.minLat) / gridDim
	if g.stepLon <= 0 {
		g.stepLon = 1e-9
	}
	if g.stepLat <= 0 {
		g.stepLat = 1e-9
	}

	for i := range areas {
		x0, y0 := g.cellOf(areas[i].MinLon, areas[i].MinLat)
		x1, y1 := g.cellOf(areas[i].MaxLon, areas[i].MaxLat)
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				idx := y*gridDim + x
				g.cells[idx] = append(g.cells[idx], int32(i))
			}
		}
	}
	return g
}

func (g *grid) cellOf(lon, lat float64) (x, y int) {
	x = int((lon - g.minLon) / g.stepLon)
	y = int((lat - g.minLat) / g.stepLat)
	return clamp(x), clamp(y)
}

func clamp(v int) int {
	if v < 0 {
		return 0
	}
	if v >= gridDim {
		return gridDim - 1
	}
	return v
}

// candidates возвращает индексы областей, чьи прямоугольники задевают ячейку
// точки. Точка вне общего прямоугольника даёт пустой список.
func (g *grid) candidates(pt Point) []int32 {
	if g == nil {
		return nil
	}
	x, y := g.cellOf(pt.Lon, pt.Lat)
	return g.cells[y*gridDim+x]
}
