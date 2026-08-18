package shadow

import (
	"testing"

	"github.com/zazzool/y-fias/internal/index"
)

func ours(names ...string) []index.Result {
	streets := make([]index.Street, 0, len(names))
	for _, n := range names {
		streets = append(streets, index.Street{Name: n, Type: "улица", Region: "77"})
	}
	snap := index.Build(streets, 1)

	out := make([]index.Result, 0, snap.Len())
	for i := range snap.Len() {
		out = append(out, index.Result{Street: snap.Street(uint32(i))})
	}
	return out
}

// Выбор человека — единственный объективный судья. Без него сравнение
// вырождается в спор о вкусах, и тест это фиксирует.
func TestCompareSuggest_WithChoice(t *testing.T) {
	cases := []struct {
		name    string
		legacy  []string
		oursTop []string
		chosen  string
		want    Verdict
	}{
		{
			name:    "выбранное у нас выше",
			legacy:  []string{"Ленинская улица", "Тверская улица"},
			oursTop: []string{"Тверская", "Ленинская"},
			chosen:  "Тверская улица",
			want:    VerdictBetter,
		},
		{
			name:    "выбранное у нас ниже",
			legacy:  []string{"Тверская улица", "Ленинская улица"},
			oursTop: []string{"Ленинская", "Тверская"},
			chosen:  "Тверская улица",
			want:    VerdictWorse,
		},
		{
			name:    "выбранного у нас нет вовсе",
			legacy:  []string{"Тверская улица"},
			oursTop: []string{"Ленинская"},
			chosen:  "Тверская улица",
			want:    VerdictWorse,
		},
		{
			name:    "мы предложили, прежняя нет",
			legacy:  []string{"Ленинская улица"},
			oursTop: []string{"Тверская", "Ленинская"},
			chosen:  "Тверская улица",
			want:    VerdictBetter,
		},
		{
			name:    "на той же позиции",
			legacy:  []string{"Тверская улица", "Ленинская улица"},
			oursTop: []string{"Тверская", "Ленинская"},
			chosen:  "Тверская улица",
			want:    VerdictSame,
		},
	}

	for _, c := range cases {
		got := CompareSuggest(
			SuggestCase{Query: "тв", Legacy: c.legacy, Chosen: c.chosen},
			ours(c.oursTop...))
		if got.Verdict != c.want {
			t.Errorf("%s: вердикт %q, ожидался %q (%s)",
				c.name, got.Verdict, c.want, got.Reason)
		}
	}
}

func TestCompareSuggest_WithoutChoice(t *testing.T) {
	cases := []struct {
		name    string
		legacy  []string
		oursTop []string
		want    Verdict
	}{
		{"обе пустые", nil, nil, VerdictSame},
		{"мы пусты", []string{"Тверская улица"}, nil, VerdictWorse},
		{"прежняя пуста", nil, []string{"Тверская"}, VerdictBetter},
		{"первые совпали", []string{"Тверская улица"}, []string{"Тверская"}, VerdictSame},
		{"первые разные", []string{"Ленинская улица"}, []string{"Тверская"}, VerdictDiffer},
	}

	for _, c := range cases {
		got := CompareSuggest(
			SuggestCase{Query: "тв", Legacy: c.legacy},
			ours(c.oursTop...))
		if got.Verdict != c.want {
			t.Errorf("%s: вердикт %q, ожидался %q (%s)",
				c.name, got.Verdict, c.want, got.Reason)
		}
	}
}

// Разное написание одной улицы — не расхождение: ключ на то и нужен.
func TestCompareSuggest_SpellingIsNotADifference(t *testing.T) {
	got := CompareSuggest(
		SuggestCase{Query: "саляма", Legacy: []string{"Саляма Адиля ул."}},
		ours("Саляма Адиля"))
	if got.Verdict != VerdictSame {
		t.Errorf("вердикт %q: «Саляма Адиля ул.» и «Саляма Адиля улица» — одна улица (%s)",
			got.Verdict, got.Reason)
	}
}

// Расхождение канонической формы дома опаснее расхождения подсказок: она
// определяет тождество адреса (§8.2).
func TestCompareHouse(t *testing.T) {
	cases := []struct {
		name         string
		legacy, ours string
		want         Verdict
	}{
		{"совпали", "7к7", "7к7", VerdictSame},
		{"регистр не важен", "7К7", "7к7", VerdictSame},
		{"мы не разобрали", "7к7", "", VerdictWorse},
		{"прежняя не разобрала", "", "7к7", VerdictBetter},
		{"формы разошлись", "7к1", "7к7", VerdictDiffer},
	}

	for _, c := range cases {
		got := CompareHouse(HouseCase{
			Input:           "7 к 7",
			LegacyCanonical: c.legacy,
			OursCanonical:   c.ours,
		})
		if got.Verdict != c.want {
			t.Errorf("%s: вердикт %q, ожидался %q (%s)",
				c.name, got.Verdict, c.want, got.Reason)
		}
	}
}

// Условие выхода этапа 3 — ноль расхождений класса «прежняя лучше».
func TestSummaryReadyToProceed(t *testing.T) {
	s := NewSummary()
	for range 10 {
		s.Add(Comparison{Verdict: VerdictSame})
	}
	s.Add(Comparison{Verdict: VerdictBetter})
	if !s.ReadyToProceed() {
		t.Error("без расхождений «прежняя лучше» переход должен быть разрешён")
	}

	s.Add(Comparison{Verdict: VerdictWorse, Query: "тверская"})
	if s.ReadyToProceed() {
		t.Error("одно расхождение «прежняя лучше» обязано блокировать переход")
	}

	if s.Total != 12 || s.Worse != 1 || s.Better != 1 || s.Same != 10 {
		t.Errorf("счётчики разошлись: %+v", s)
	}
	if len(s.Samples[VerdictWorse]) != 1 {
		t.Error("пример блокирующего расхождения не сохранён")
	}
}

// Пустая сводка не означает готовности: сравнивать было нечего.
func TestEmptySummaryIsNotReady(t *testing.T) {
	if NewSummary().ReadyToProceed() {
		t.Error("пустая сводка не должна разрешать переход")
	}
}
