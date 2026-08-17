/**
 * Org Matcher — сопоставление адреса вызова организации.
 *
 * Организации (orgs) имеют группу адресов-масок (org_addresses).
 * Семантика маски дома:
 *  - пустой house            → любой дом на этой улице;
 *  - house без строения («2») → все строения/корпуса той же базы
 *    (2, 2с17, 2к5, 2/44кГ при базе «2/44»), но не «20»;
 *  - house со строением («3с7») → только точное совпадение.
 *
 * Справочник кэшируется в памяти; кэш сбрасывается при изменении
 * организаций через админку (invalidateOrgMatcher) и по TTL.
 */

import { getPostgresPool } from '../db/postgres'
import { formatHouseCanonical } from './addressService'

export interface MatchedOrg {
	id: string
	text: string
	type: string | null
	priority: number
}

interface OrgAddressMask {
	org: MatchedOrg
	houseNorm: string // '' = вся улица
	houseBase: string
	houseNum: string // ведущее число дома, '' если его нет
	isExactMask: boolean // маска содержит строение/корпус
}

/* ── Нормализация ── */

// Токены-типы улиц. Важно: не через \b-регэкспы — в JS \b не работает
// с кириллицей, поэтому фильтруем по целым токенам.
const STREET_TYPE_TOKENS = new Set([
	'ул', 'улица', 'ш', 'шоссе', 'пр', 'пркт', 'проспект', 'пер', 'переулок',
	'пл', 'площадь', 'бр', 'бульвар', 'наб', 'набережная', 'прд', 'проезд',
	'туп', 'тупик', 'аллея', 'линия'
])

/** Улица: без типа (ул./шоссе/пер…), ё→е, порядок слов не важен */
export function normalizeStreetKey(str: string): string {
	const tokens = String(str || '')
		.toLowerCase()
		.replace(/ё/g, 'е')
		.replace(/й/g, 'и')
		.replace(/\(.*?\)/g, ' ')
		.replace(/[.,\-/\\]+/g, ' ')
		.split(/\s+/)
		.filter(t => t && !STREET_TYPE_TOKENS.has(t))
	return tokens.sort().join(' ')
}

/** Дом: канонизация (стр→с, корп→к, без пробелов/точек) + lower */
export function normalizeHouseKey(h: string): string {
	if (!h) return ''
	return formatHouseCanonical(String(h)).toLowerCase().replace(/\\/g, '/')
}

/**
 * База дома: часть до первого маркера строения/корпуса/владения,
 * без хвостовой литеры: «2/44кГ»→«2/44», «10а»→«10», «15кабк»→«15».
 */
export function houseBase(normHouse: string): string {
	if (!normHouse) return ''
	// отрезаем всё, начиная с маркера с/к/вл, за которым идёт цифра или буква
	const m = normHouse.match(/^(.*?)(?:(?:вл|с|к)[0-9а-я].*)?$/)
	let base = m ? m[1] : normHouse
	// хвостовая литера дома («10а», «2/44г») не входит в базу
	base = base.replace(/[а-я]+$/, '')
	// «2/44к-а»: буква корпуса отделена дефисом, и до неё регэксп не доходит —
	// оставался хвост «2/44к-», который не совпадал ни с одной маской.
	base = base.replace(/[\s\-,]+$/, '').replace(/[а-я]+$/, '').replace(/[\s\-,]+$/, '')
	return base || normHouse
}

/**
 * Ведущее число дома: «2/44с1кГ» → 2, «15к1» → 15, «10А» → 10.
 * Тот же приём, что в SQL-функции house_num() (миграция 0026). Нужен как
 * последняя ступень: маска может быть записана как «2», а адрес вызова — как
 * «2/44с1кГ» (угловой дом 2 по одной улице и 44 по другой). База у них разная
 * («2» против «2/44»), и без сравнения по числу организация терялась.
 */
export function houseLeadingNum(normHouse: string): string {
	const m = String(normHouse || '').match(/^\s*(\d+)/)
	return m ? m[1] : ''
}

/* ── Кэш справочника ── */

const CACHE_TTL_MS = 5 * 60_000

let cache: Map<string, OrgAddressMask[]> | null = null
let cacheLoadedAt = 0
let loading: Promise<Map<string, OrgAddressMask[]>> | null = null

export function invalidateOrgMatcher() {
	cache = null
	cacheLoadedAt = 0
}

async function loadIndex(): Promise<Map<string, OrgAddressMask[]>> {
	const pool = getPostgresPool()
	const { rows } = await pool.query(
		`SELECT oa.street, oa.house, o.id, o.text, o.type, COALESCE(o.priority, 0) AS priority
		 FROM org_addresses oa
		 JOIN orgs o ON o.id = oa.org_id`
	)

	const index = new Map<string, OrgAddressMask[]>()
	for (const r of rows) {
		const streetKey = normalizeStreetKey(r.street)
		if (!streetKey) continue
		const houseNorm = normalizeHouseKey(r.house || '')
		const mask: OrgAddressMask = {
			org: { id: r.id, text: r.text, type: r.type, priority: Number(r.priority) || 0 },
			houseNorm,
			houseBase: houseBase(houseNorm),
			houseNum: houseLeadingNum(houseNorm),
			isExactMask: houseNorm !== '' && houseNorm !== houseBase(houseNorm)
		}
		const list = index.get(streetKey)
		if (list) list.push(mask)
		else index.set(streetKey, [mask])
	}
	return index
}

async function getIndex(): Promise<Map<string, OrgAddressMask[]>> {
	if (cache && Date.now() - cacheLoadedAt < CACHE_TTL_MS) return cache
	if (!loading) {
		loading = loadIndex()
			.then(idx => {
				cache = idx
				cacheLoadedAt = Date.now()
				return idx
			})
			.finally(() => {
				loading = null
			})
	}
	return loading
}

/* ── Сопоставление ── */

function scoreMask(
	mask: OrgAddressMask,
	houseNorm: string,
	base: string,
	num: string
): number {
	if (mask.houseNorm === '') return houseNorm ? 1 : 2 // вся улица — слабое совпадение
	if (mask.houseNorm === houseNorm) return 5 // точное
	if (!mask.isExactMask && mask.houseBase === base && base !== '') return 4 // база
	// Сравнение по ведущему числу — слабее базы, но сильнее «всей улицы»:
	// «2» и «2/44с1кГ» это один дом, а «2» и «20» — разные.
	if (!mask.isExactMask && mask.houseNum !== '' && mask.houseNum === num) return 3
	return 0
}

/** Синхронный матч по уже загруженному индексу (для пакетного обогащения) */
function matchInIndex(
	index: Map<string, OrgAddressMask[]>,
	street: string,
	house: string
): MatchedOrg | null {
	const streetKey = normalizeStreetKey(street)
	if (!streetKey) return null
	const masks = index.get(streetKey)
	if (!masks?.length) return null

	const houseNorm = normalizeHouseKey(house)
	const base = houseBase(houseNorm)
	const num = houseLeadingNum(houseNorm)

	let best: MatchedOrg | null = null
	let bestScore = 0
	for (const mask of masks) {
		const s = scoreMask(mask, houseNorm, base, num)
		if (s > bestScore || (s === bestScore && best && mask.org.priority > best.priority)) {
			if (s > 0) {
				best = mask.org
				bestScore = s
			}
		}
	}
	return best
}

/** Найти организацию по адресу (улица + дом) */
export async function matchOrgByAddress(street: string, house: string): Promise<MatchedOrg | null> {
	if (!street) return null
	try {
		return matchInIndex(await getIndex(), street, house)
	} catch (e) {
		console.error('[orgMatcher] match error:', e)
		return null
	}
}

/**
 * Пакетное обогащение списка вызовов: проставляет a_org по адресу,
 * если организация не указана вручную. Мутирует переданные объекты.
 */
export async function enrichCallsWithOrgs(calls: Array<Record<string, any>>): Promise<void> {
	if (!calls?.length) return
	let index: Map<string, OrgAddressMask[]>
	try {
		index = await getIndex()
	} catch (e) {
		console.error('[orgMatcher] index load error:', e)
		return
	}
	for (const c of calls) {
		if (c.a_org || !c.a_str) continue
		const org = matchInIndex(index, c.a_str, c.a_h || '')
		if (org) c.a_org = org.text
	}
}
