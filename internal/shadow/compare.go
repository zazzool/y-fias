// Package shadow сравнивает ответы прежней системы с нашими.
//
// Этап 3 плана: сервис работает РЯДОМ с существующим бэкендом, его ответы
// игнорируются, а расхождения записываются. Решение «идти дальше» принимается
// на неделе реальных данных, а не на вере в дизайн.
//
// Сравнение живёт здесь, а не в МИС, намеренно. Во-первых, оно тестируемо:
// классификация расхождений — самая содержательная часть теневого режима, и
// её надо покрывать тестами, а не наблюдать в чужих логах. Во-вторых, правка
// чужого репозитория сводится к вызову «отправь и забудь».
package shadow

import (
	"strings"

	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/normalize"
)

// Verdict — итог сравнения.
type Verdict string

const (
	// VerdictSame — ответы совпали по существу.
	VerdictSame Verdict = "same"
	// VerdictBetter — мы нашли то, чего прежняя система не нашла, либо
	// поставили нужное выше.
	VerdictBetter Verdict = "better"
	// VerdictWorse — прежняя система нашла лучше. ЭТО БЛОКИРУЮЩИЙ КЛАСС:
	// условие выхода этапа 3 — ноль таких расхождений.
	VerdictWorse Verdict = "worse"
	// VerdictDiffer — ответы разные, но сказать, чей лучше, нельзя.
	VerdictDiffer Verdict = "differ"
)

// Comparison — результат сравнения одного запроса.
type Comparison struct {
	Verdict Verdict
	Reason  string

	Query      string
	LegacyTop  string
	OursTop    string
	LegacyRank int // позиция выбранного в выдаче прежней системы, -1 если нет
	OursRank   int
}

// SuggestCase — данные для сравнения подсказок.
type SuggestCase struct {
	Query string

	// Legacy — что показала прежняя система, по порядку.
	Legacy []string

	// Chosen — что человек выбрал в итоге, если известно.
	//
	// Это единственный объективный судья: не «чей список красивее», а «чей
	// список содержал нужное и выше». Без выбора человека сравнение
	// вырождается в спор о вкусах.
	Chosen string
}

// CompareSuggest сравнивает выдачу подсказок.
func CompareSuggest(c SuggestCase, ours []index.Result) Comparison {
	cmp := Comparison{
		Query:      c.Query,
		LegacyRank: -1,
		OursRank:   -1,
	}

	oursKeys := make([]string, 0, len(ours))
	for _, r := range ours {
		oursKeys = append(oursKeys, r.Street.Key)
	}
	legacyKeys := make([]string, 0, len(c.Legacy))
	for _, s := range c.Legacy {
		legacyKeys = append(legacyKeys, normalize.StreetKey(s))
	}

	if len(c.Legacy) > 0 {
		cmp.LegacyTop = c.Legacy[0]
	}
	if len(ours) > 0 {
		cmp.OursTop = ours[0].Street.Full()
	}

	// Без выбора человека судить не о чем: сравниваем только наличие ответа.
	if c.Chosen == "" {
		switch {
		case len(ours) == 0 && len(c.Legacy) == 0:
			cmp.Verdict, cmp.Reason = VerdictSame, "обе системы ничего не нашли"
		case len(ours) == 0:
			cmp.Verdict, cmp.Reason = VerdictWorse, "мы не нашли ничего, прежняя нашла"
		case len(c.Legacy) == 0:
			cmp.Verdict, cmp.Reason = VerdictBetter, "мы нашли, прежняя не нашла"
		case sameKey(cmp.OursTop, cmp.LegacyTop):
			cmp.Verdict, cmp.Reason = VerdictSame, "первые совпали"
		default:
			cmp.Verdict, cmp.Reason = VerdictDiffer, "первые разные, выбор человека неизвестен"
		}
		return cmp
	}

	chosenKey := normalize.StreetKey(c.Chosen)
	cmp.LegacyRank = rankOf(legacyKeys, chosenKey)
	cmp.OursRank = rankOf(oursKeys, chosenKey)

	switch {
	case cmp.OursRank < 0 && cmp.LegacyRank < 0:
		cmp.Verdict = VerdictSame
		cmp.Reason = "выбранного нет ни в одной выдаче"

	case cmp.OursRank < 0:
		// Самый тяжёлый случай: человек выбрал то, чего мы не предложили.
		cmp.Verdict = VerdictWorse
		cmp.Reason = "выбранного нет в нашей выдаче, а в прежней он был"

	case cmp.LegacyRank < 0:
		cmp.Verdict = VerdictBetter
		cmp.Reason = "мы предложили выбранное, прежняя не предложила"

	case cmp.OursRank < cmp.LegacyRank:
		cmp.Verdict = VerdictBetter
		cmp.Reason = "выбранное у нас выше"

	case cmp.OursRank > cmp.LegacyRank:
		cmp.Verdict = VerdictWorse
		cmp.Reason = "выбранное у нас ниже"

	default:
		cmp.Verdict = VerdictSame
		cmp.Reason = "выбранное на той же позиции"
	}
	return cmp
}

// HouseCase — данные для сравнения разбора номера дома.
type HouseCase struct {
	Input string
	// LegacyCanonical — каноническая форма из прежней системы
	// (formatHouseCanonical).
	LegacyCanonical string
	// OursCanonical — наша.
	OursCanonical string
}

// CompareHouse сравнивает канонизацию номера дома.
//
// Расхождение здесь опаснее, чем в подсказках: каноническая форма определяет
// ТОЖДЕСТВО адреса (§8.2). Если она разошлась, при переключении записи
// (этап 5) адреса начнут слипаться или, наоборот, дублироваться.
func CompareHouse(c HouseCase) Comparison {
	cmp := Comparison{
		Query:     c.Input,
		LegacyTop: c.LegacyCanonical,
		OursTop:   c.OursCanonical,
	}

	switch {
	case strings.EqualFold(c.LegacyCanonical, c.OursCanonical):
		cmp.Verdict, cmp.Reason = VerdictSame, "канонические формы совпали"

	case c.OursCanonical == "" && c.LegacyCanonical != "":
		cmp.Verdict = VerdictWorse
		cmp.Reason = "мы не разобрали номер, прежняя разобрала"

	case c.LegacyCanonical == "" && c.OursCanonical != "":
		cmp.Verdict = VerdictBetter
		cmp.Reason = "мы разобрали номер, прежняя не разобрала"

	default:
		// Не берёмся судить, чья форма «правильнее»: обе могут быть
		// осмысленными. Но расхождение обязано попасть в отчёт — при
		// переключении записи оно определит, слипнутся адреса или нет.
		cmp.Verdict = VerdictDiffer
		cmp.Reason = "канонические формы разошлись; влияет на тождество адреса"
	}
	return cmp
}

func rankOf(keys []string, want string) int {
	for i, k := range keys {
		if k == want {
			return i
		}
	}
	return -1
}

func sameKey(a, b string) bool {
	return normalize.StreetKey(a) == normalize.StreetKey(b)
}

/* ── Сводка ────────────────────────────────────────────────────── */

// Summary — накопленная статистика теневого прогона.
type Summary struct {
	Total   int
	Same    int
	Better  int
	Worse   int
	Differ  int
	Samples map[Verdict][]Comparison
}

func NewSummary() *Summary {
	return &Summary{Samples: map[Verdict][]Comparison{}}
}

// Add учитывает сравнение, сохраняя до нескольких примеров каждого класса.
func (s *Summary) Add(c Comparison) {
	s.Total++
	switch c.Verdict {
	case VerdictSame:
		s.Same++
	case VerdictBetter:
		s.Better++
	case VerdictWorse:
		s.Worse++
	default:
		s.Differ++
	}

	const keepSamples = 20
	if len(s.Samples[c.Verdict]) < keepSamples {
		s.Samples[c.Verdict] = append(s.Samples[c.Verdict], c)
	}
}

// ReadyToProceed — выполнено ли условие выхода этапа 3.
//
// Оно одно: расхождений класса «прежняя нашла лучше» должно быть НОЛЬ.
// Класса «мы нашли лучше» может быть сколько угодно.
func (s *Summary) ReadyToProceed() bool { return s.Worse == 0 && s.Total > 0 }
