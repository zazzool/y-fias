package index

// Нечёткое сравнение строк.
//
// Пороги перенесены из server/domain/streetDictionary.ts БЕЗ изменений:
// расстояние Левенштейна <= 2 при отношении длин >= 0.6, либо биграммный
// коэффициент Дайса >= 0.65 для строк от 5 символов. Значения проверены
// практикой на реальных диспетчерах, и менять их можно только по результатам
// замеров на golden-корпусе, а не из соображений красоты.

const (
	// MaxEditDistance — максимум правок для признания сходства.
	MaxEditDistance = 2
	// MinLengthRatio — при большей разнице длин расстояние не считается.
	MinLengthRatio = 0.6
	// MinDiceScore — порог биграммного сходства.
	MinDiceScore = 0.65
	// MinDiceLength — от какой длины применяется биграммное сходство.
	MinDiceLength = 5
)

// bigramsOf разбивает строку на биграммы, упакованные в uint32.
//
// Упаковка двух рун в одно число избавляет от аллокации строк на горячем
// пути: карта биграмм строится один раз на снапшот, а запросов к ней много.
func bigramsOf(s string) []uint32 {
	runes := []rune(s)
	if len(runes) < 2 {
		return nil
	}
	out := make([]uint32, 0, len(runes)-1)
	for i := 0; i+1 < len(runes); i++ {
		out = append(out, packBigram(runes[i], runes[i+1]))
	}
	return out
}

func packBigram(a, b rune) uint32 {
	// Рунам кириллицы и латиницы хватает 16 бит; редкие символы вне
	// диапазона схлопываются, что для нечёткого слоя допустимо.
	return uint32(uint16(a))<<16 | uint32(uint16(b))
}

// DiceSimilarity — коэффициент Дайса по биграммам: 2|A∩B| / (|A|+|B|).
func DiceSimilarity(a, b string) float64 {
	ba, bb := bigramsOf(a), bigramsOf(b)
	if len(ba) == 0 || len(bb) == 0 {
		if a == b {
			return 1
		}
		return 0
	}

	counts := make(map[uint32]int, len(ba))
	for _, g := range ba {
		counts[g]++
	}

	var common int
	for _, g := range bb {
		if counts[g] > 0 {
			counts[g]--
			common++
		}
	}
	return 2 * float64(common) / float64(len(ba)+len(bb))
}

// EditDistance — расстояние Дамерау—Левенштейна с ограничением сверху.
//
// Ограничение существенно: без него на каждую пару строк тратится O(n*m), а
// кандидатов после биграммного отбора могут быть сотни. Как только становится
// ясно, что расстояние превысит max, вычисление прекращается.
func EditDistance(a, b string, maxDist int) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	if abs(len(ra)-len(rb)) > maxDist {
		return maxDist + 1
	}

	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	prevPrev := make([]int, len(rb)+1)

	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		rowMin := cur[0]

		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d := min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)

			// Транспозиция: «Тверская» и «Твесркая» отличаются на одну
			// перестановку, а не на две правки.
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d = min(d, prevPrev[j-2]+cost)
			}

			cur[j] = d
			rowMin = min(rowMin, d)
		}

		if rowMin > maxDist {
			return maxDist + 1
		}
		prevPrev, prev, cur = prev, cur, prevPrev
	}

	return prev[len(rb)]
}

// IsSimilar применяет оба правила из streetDictionary.ts.
// Возвращает признак сходства и его меру от 0 до 1.
func IsSimilar(query, candidate string) (bool, float64) {
	rq, rc := []rune(query), []rune(candidate)
	if len(rq) == 0 || len(rc) == 0 {
		return false, 0
	}

	// Правило 1: близкое расстояние при сопоставимой длине.
	ratio := float64(min(len(rq), len(rc))) / float64(max(len(rq), len(rc)))
	if ratio >= MinLengthRatio {
		if d := EditDistance(query, candidate, MaxEditDistance); d <= MaxEditDistance {
			return true, 1 - float64(d)/float64(MaxEditDistance+1)
		}
	}

	// Правило 2: биграммное сходство для строк подлиннее.
	if len(rq) >= MinDiceLength && len(rc) >= MinDiceLength {
		if dice := DiceSimilarity(query, candidate); dice >= MinDiceScore {
			return true, dice
		}
	}

	return false, 0
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
