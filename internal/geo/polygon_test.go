package geo

import "testing"

// Квадрат со стороной 1 и дырой в середине.
func squareWithHole() Area {
	a := Area{
		Name: "квадрат",
		Polygons: []Polygon{{
			Outer: Ring{{0, 0}, {10, 0}, {10, 10}, {0, 10}, {0, 0}},
			Inner: []Ring{{{4, 4}, {6, 4}, {6, 6}, {4, 6}, {4, 4}}},
		}},
	}
	a.bbox()
	return a
}

func TestContains(t *testing.T) {
	a := squareWithHole()

	cases := []struct {
		name string
		pt   Point
		want bool
	}{
		{"внутри", Point{2, 2}, true},
		{"внутри у края", Point{9.5, 9.5}, true},
		{"в дыре", Point{5, 5}, false},
		{"снаружи справа", Point{11, 5}, false},
		{"снаружи снизу", Point{5, -1}, false},
		{"далеко", Point{100, 100}, false},
	}
	for _, c := range cases {
		if got := a.Contains(c.pt); got != c.want {
			t.Errorf("%s: Contains(%v) = %v, ожидалось %v", c.name, c.pt, got, c.want)
		}
	}
}

func TestIndexLocate(t *testing.T) {
	left := Area{Name: "левый", Polygons: []Polygon{{
		Outer: Ring{{0, 0}, {5, 0}, {5, 10}, {0, 10}, {0, 0}},
	}}}
	right := Area{Name: "правый", Polygons: []Polygon{{
		Outer: Ring{{5, 0}, {10, 0}, {10, 10}, {5, 10}, {5, 0}},
	}}}

	ix := NewIndex([]Area{left, right})
	if ix.Len() != 2 {
		t.Fatalf("областей %d, ожидалось 2", ix.Len())
	}

	if a, _ := ix.Locate(Point{2, 5}); a == nil || a.Name != "левый" {
		t.Errorf("точка слева попала в %v", a)
	}
	if a, _ := ix.Locate(Point{7, 5}); a == nil || a.Name != "правый" {
		t.Errorf("точка справа попала в %v", a)
	}
	if a, _ := ix.Locate(Point{20, 20}); a != nil {
		t.Errorf("точка снаружи попала в %q", a.Name)
	}
}

// Кольцо, собранное из кусков в произвольном порядке и направлении, должно
// давать тот же результат, что и собранное подряд. Именно здесь ломается
// наивная склейка: ломаная выходит самопересекающейся, и проверка попадания
// начинает молча врать.
func TestStitchHandlesOrderAndDirection(t *testing.T) {
	// Квадрат из четырёх сторон, поданных вразнобой и с переворотами.
	segs := [][]Point{
		{{10, 10}, {0, 10}}, // верх, справа налево
		{{0, 0}, {10, 0}},   // низ, слева направо
		{{0, 10}, {0, 0}},   // лево, сверху вниз
		{{10, 0}, {10, 10}}, // право, снизу вверх
	}

	rings := stitch(segs)
	if len(rings) != 1 {
		t.Fatalf("колец %d, ожидалось 1: %v", len(rings), rings)
	}

	a := Area{Polygons: []Polygon{{Outer: rings[0]}}}
	a.bbox()

	if !a.Contains(Point{5, 5}) {
		t.Error("центр квадрата оказался снаружи — кольцо собрано неверно")
	}
	if a.Contains(Point{15, 5}) {
		t.Error("точка вне квадрата оказалась внутри")
	}
}

func TestCleanAreaName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"район Кунцево", "Кунцево"},
		{"Мещанский район", "Мещанский"},
		{"муниципальный округ Басманный", "Басманный"},
		{"городской округ Троицк", "Троицк"},
		{"поселение Внуковское", "Внуковское"},
		{"Хорошёво-Мнёвники", "Хорошёво-Мнёвники"},
		{"", ""},
	}
	for _, c := range cases {
		if got := CleanAreaName(c.in); got != c.want {
			t.Errorf("CleanAreaName(%q) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

func BenchmarkLocate(b *testing.B) {
	// Сетка 20x20 областей — грубая замена реальным 198.
	var areas []Area
	for x := 0; x < 20; x++ {
		for y := 0; y < 20; y++ {
			fx, fy := float64(x), float64(y)
			areas = append(areas, Area{
				Polygons: []Polygon{{Outer: Ring{
					{fx, fy}, {fx + 1, fy}, {fx + 1, fy + 1}, {fx, fy + 1}, {fx, fy},
				}}},
			})
		}
	}
	ix := NewIndex(areas)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		ix.Locate(Point{10.5, 10.5})
	}
}
