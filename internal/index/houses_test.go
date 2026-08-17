package index

import (
	"testing"

	"github.com/zazzool/y-fias/internal/parse"
)

func withHouses(t *testing.T) *Snapshot {
	t.Helper()

	s := Build([]Street{
		{Name: "Саляма Адиля", Type: "улица", Region: "77"},
		{Name: "Пустая", Type: "улица", Region: "77"},
	}, 1)

	s.SetHouses(map[uint32][]string{
		0: {"7", "7к1", "7к7", "10", "10с4", "2/44кг", "15а", "100"},
	})
	return s
}

func TestLookupHouse(t *testing.T) {
	s := withHouses(t)

	cases := []struct {
		street uint32
		house  string
		want   parse.HouseQC
	}{
		// Дом есть в справочнике.
		{0, "7", parse.HouseInGar},
		{0, "7к7", parse.HouseInGar},
		{0, "7 к 7", parse.HouseInGar}, // другое написание того же дома
		{0, "10с4", parse.HouseInGar},
		{0, "15А", parse.HouseInGar},

		// Дома с таким числом есть, но нашего среди них нет.
		// Это НЕ отказ: справочник ФНС неполон, вызов туда приедет.
		{0, "7к9", parse.HouseNotInGar},
		{0, "10с9", parse.HouseNotInGar},
		{0, "55", parse.HouseNotInGar},

		// Дома улицы не загружены — сказать нечего.
		{1, "7", parse.HouseUnknown},
	}

	for _, c := range cases {
		got := s.LookupHouse(c.street, parse.ParseHouse(c.house))
		if got != c.want {
			t.Errorf("LookupHouse(улица %d, %q) = %d, ожидалось %d",
				c.street, c.house, got, c.want)
		}
	}
}

// Регресс §8.2 на уровне справочника: «7к7» и «7к1» — разные дома, и наличие
// одного не делает существующим другой.
func TestLookupHouse_Issue_8_2(t *testing.T) {
	s := withHouses(t)

	if got := s.LookupHouse(0, parse.ParseHouse("7к7")); got != parse.HouseInGar {
		t.Fatalf("«7к7» есть в справочнике, а LookupHouse вернул %d", got)
	}
	if got := s.LookupHouse(0, parse.ParseHouse("7к3")); got != parse.HouseNotInGar {
		t.Errorf("«7к3» в справочнике нет, а LookupHouse вернул %d — "+
			"вероятно, сравнение идёт по ведущему числу", got)
	}
}

func TestNeighborsOf(t *testing.T) {
	s := withHouses(t)

	// Вокруг дома 10 ближайшие по номеру — сами десятки и семёрки.
	got := s.NeighborsOf(0, 10, 4)
	if len(got) != 4 {
		t.Fatalf("соседей %d, ожидалось 4", len(got))
	}
	for _, n := range got {
		if n.Num != 7 && n.Num != 10 {
			t.Errorf("сосед %q (число %d) слишком далёк от дома 10", n.Canonical, n.Num)
		}
	}

	// Запрос больше, чем есть домов, не должен паниковать.
	if all := s.NeighborsOf(0, 10, 100); len(all) != 8 {
		t.Errorf("вернулось %d соседей, а домов всего 8", len(all))
	}
	if none := s.NeighborsOf(1, 10, 5); none != nil {
		t.Errorf("у улицы без домов соседей быть не должно, вернулось %d", len(none))
	}
}

func TestHousesSortedByNumber(t *testing.T) {
	s := withHouses(t)

	list := s.HousesOf(0)
	if len(list) != 8 {
		t.Fatalf("домов %d, ожидалось 8", len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].Num > list[i].Num {
			t.Errorf("порядок нарушен: %q (%d) перед %q (%d)",
				list[i-1].Canonical, list[i-1].Num, list[i].Canonical, list[i].Num)
		}
	}
	if s.HouseCount() != 8 {
		t.Errorf("HouseCount = %d, ожидалось 8", s.HouseCount())
	}
}
