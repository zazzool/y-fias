package index

import (
	"testing"

	"github.com/zazzool/y-fias/internal/normalize"
)

// TestDiag_WhoCompetes — диагностика, а не проверка. Показывает, кто занимает
// первые места при коротком вводе и почему нужная улица туда не попадает.
// Запуск: go test -run TestDiag_WhoCompetes -v ./internal/index/
func TestDiag_WhoCompetes(t *testing.T) {
	if testing.Short() {
		t.Skip("диагностика")
	}
	s := loadGoldenSnapshot(t)

	// Возьмём улицы, которым нужно 4+ буквы, и посмотрим на конкурентов.
	var slow []*Street
	for i := 0; i < s.Len() && len(slow) < 3; i++ {
		st := s.Street(uint32(i))
		runes := []rune(normalize.SearchNorm(st.Name))
		if len(runes) < 6 {
			continue
		}
		need := -1
		for n := 1; n <= len(runes); n++ {
			for _, r := range s.Suggest(Query{Text: string(runes[:n]), Limit: 5}) {
				if r.Street.Key == st.Key {
					need = n
					break
				}
			}
			if need > 0 {
				break
			}
		}
		if need >= 4 {
			slow = append(slow, st)
		}
	}

	for _, st := range slow {
		runes := []rune(normalize.SearchNorm(st.Name))
		q := string(runes[:2])
		t.Logf("\n=== цель: %q (вызовов %d), ввод %q", st.Full(), st.Calls, q)
		for i, r := range s.Suggest(Query{Text: q, Limit: 8}) {
			mark := " "
			if r.Street.Key == st.Key {
				mark = ">"
			}
			t.Logf("  %s %d. %-42s балл %6.2f слой %-9s вызовов %d",
				mark, i+1, r.Street.Full(), r.Score, r.Layer, r.Street.Calls)
		}
	}

	// Сколько всего кандидатов на однобуквенный и двухбуквенный ввод.
	for _, q := range []string{"п", "по", "к", "ко"} {
		all := s.Suggest(Query{Text: q, Limit: 10000})
		t.Logf("ввод %q -> кандидатов %d", q, len(all))
	}
}
