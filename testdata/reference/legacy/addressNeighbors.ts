/**
 * Вывод района и ПНД по соседним домам той же улицы.
 *
 * Идея: если на улице уже есть адреса с известным районом, то новый дом почти
 * наверняка относится к тому же району. Правила выведены из устройства
 * московской адресации:
 *
 * 1. Дома с модификаторами («7к2», «7с1», «вл7») стоят вплотную к дому-родителю
 *    («7») — совпадение ЧИСЛА дома даёт максимальный вес.
 * 2. Чётная и нечётная стороны большой улицы могут относиться к разным районам,
 *    поэтому соседи ТОЙ ЖЕ чётности весят существенно больше соседей напротив.
 * 3. Чем дальше номер, тем слабее свидетельство: длинные улицы (шоссе,
 *    проспекты) пересекают несколько районов.
 *
 * Модуль чистый (без БД) — вся логика тестируема на массивах строк.
 */

export interface NeighborRow {
	/** Номер дома как записан в БД */
	house: string | null
	/** Район адреса */
	district: string | null
	/** ПНД адреса */
	pnd_id: number | null
}

export interface NeighborCandidate {
	dis: string
	pnd: number | null
	/** Суммарный вес свидетельств */
	weight: number
	/** Сколько адресов поддерживают кандидата */
	support: number
}

export interface NeighborInference {
	dis: string
	pnd: number | null
	/**
	 * high — можно подставлять автоматически;
	 * low — только как кандидат для уточнения.
	 */
	confidence: 'high' | 'low'
	candidates: NeighborCandidate[]
}

/** Первое число в номере дома: «7к7» → 7, «вл15А» → 15, «СЕМЬк7» → 7, «дом» → null */
export function houseLeadingNum(h: string | null | undefined): number | null {
	if (!h) return null
	const m = String(h).match(/(\d+)/)
	if (!m) return null
	const n = parseInt(m[1], 10)
	return Number.isFinite(n) ? n : null
}

/** Вес свидетельства одного соседа. 0 — сосед не учитывается. */
export function neighborWeight(inputNum: number, neighborNum: number): number {
	if (inputNum === neighborNum) return 100 // корпус/строение у дома-родителя
	const d = Math.abs(inputNum - neighborNum)
	const sameParity = inputNum % 2 === neighborNum % 2
	if (sameParity) return Math.max(0, 50 - 4 * d)
	// Противоположная сторона улицы: теоретически другой район — вес низкий
	return Math.max(0, 20 - 4 * d)
}

/**
 * Вывести район/ПНД из соседей. Возвращает null, когда свидетельств нет
 * (нет числа в номере дома или нет пригодных соседей).
 */
export function inferFromNeighbors(
	inputHouse: string,
	rows: NeighborRow[]
): NeighborInference | null {
	const num = houseLeadingNum(inputHouse)
	if (num == null || !rows?.length) return null

	// Агрегация весов по районам
	const byDis = new Map<string, { weight: number; support: number; pnds: Map<number, number> }>()

	for (const row of rows) {
		const dis = String(row.district || '').trim()
		if (!dis) continue
		const rn = houseLeadingNum(row.house)
		if (rn == null) continue
		const w = neighborWeight(num, rn)
		if (w <= 0) continue

		let agg = byDis.get(dis)
		if (!agg) {
			agg = { weight: 0, support: 0, pnds: new Map() }
			byDis.set(dis, agg)
		}
		agg.weight += w
		agg.support += 1
		if (row.pnd_id != null && row.pnd_id !== 0) {
			agg.pnds.set(row.pnd_id, (agg.pnds.get(row.pnd_id) || 0) + w)
		}
	}

	if (!byDis.size) return null

	const candidates: NeighborCandidate[] = [...byDis.entries()]
		.map(([dis, agg]) => ({
			dis,
			pnd: modePnd(agg.pnds),
			weight: agg.weight,
			support: agg.support
		}))
		.sort((a, b) => b.weight - a.weight)

	const top = candidates[0]
	const total = candidates.reduce((s, c) => s + c.weight, 0)
	const share = total > 0 ? top.weight / total : 0

	// Автоподстановка только при выраженном консенсусе:
	// либо совпало число дома (родитель/корпуса, вес ≥ 100),
	// либо не меньше двух согласных соседей — и в обоих случаях
	// конкуренты дают не более 20% суммарного веса.
	const strongEvidence = top.weight >= 100 || top.support >= 2
	const confidence: 'high' | 'low' = share >= 0.8 && strongEvidence ? 'high' : 'low'

	return {
		dis: top.dis,
		pnd: top.pnd,
		confidence,
		candidates
	}
}

/** ПНД с наибольшим суммарным весом; null, если ни у кого не заполнен. */
function modePnd(pnds: Map<number, number>): number | null {
	let best: number | null = null
	let bestW = 0
	for (const [pnd, w] of pnds) {
		if (w > bestW) {
			bestW = w
			best = pnd
		}
	}
	return best
}
