import { ofetch } from 'ofetch'
import { useRuntimeConfig } from '#imports'

interface DaDataSuggestion {
	value: string
	unrestricted_value: string
	data: any
}

interface DaDataResponse {
	suggestions: DaDataSuggestion[]
}

/* ── LRU Cache with TTL ── */

const CACHE_MISS = Symbol('CACHE_MISS')
type CacheEntry<T> = { expiresAt: number; data: T }

const CACHE_TTL_MS = 3 * 60 * 1000
const CACHE_MAX_SIZE = 2000

const cache = new Map<string, CacheEntry<any>>()

function getCache<T>(key: string): T | typeof CACHE_MISS {
	const entry = cache.get(key)
	if (!entry) return CACHE_MISS
	if (entry.expiresAt < Date.now()) {
		cache.delete(key)
		return CACHE_MISS
	}
	// LRU: move to end
	cache.delete(key)
	cache.set(key, entry)
	return entry.data as T
}

function setCache<T>(key: string, data: T): void {
	// Evict oldest entries if at capacity
	if (cache.size >= CACHE_MAX_SIZE) {
		const first = cache.keys().next().value
		if (first !== undefined) cache.delete(first)
	}
	cache.set(key, { data, expiresAt: Date.now() + CACHE_TTL_MS })
}

/* ── Token management ── */

let lastTokenIndex = 0

function getSugTokens(raw: string): string[] {
	return raw
		.split(' ')
		.map(t => t.trim())
		.filter(Boolean)
}

const DADATA_TIMEOUT_MS = 4000
const NOMINATIM_TIMEOUT_MS = 5000
const NOMINATIM_USER_AGENT = 'MIS-Vyzov/1.0 (psychiatric-emergency-dispatch)'

async function callSugEndpoint(
	url: string,
	token: string,
	payload: any
): Promise<DaDataResponse | null> {
	try {
		return await ofetch<DaDataResponse>(url, {
			method: 'POST',
			headers: {
				'Content-Type': 'application/json',
				Authorization: `Token ${token}`
			},
			body: payload,
			timeout: DADATA_TIMEOUT_MS
		})
	} catch (e: any) {
		if (e?.response?.status !== 429) {
			console.error('[DaData] suggest error:', e?.response?.status || e?.code || 'unknown')
		}
		return null
	}
}

async function callPaidEndpoint(
	url: string,
	token: string,
	secret: string,
	query: string
): Promise<any | null> {
	if (!url || !token || !secret) return null
	try {
		return await ofetch<any>(url, {
			method: 'POST',
			headers: {
				'Content-Type': 'application/json',
				Authorization: `Token ${token}`,
				'X-Secret': `${secret}`
			},
			body: [query],
			timeout: DADATA_TIMEOUT_MS
		}).then((data: any[]) => data?.[0] || null)
	} catch (e: any) {
		console.error('[DaData] paid error:', e?.response?.status || e?.code || 'unknown')
		return null
	}
}

export async function getStreetSuggestions(query: string): Promise<string[]> {
	const config = useRuntimeConfig()
	const baseUrl = config.dadadataSugUrl
	const tokens = getSugTokens(config.dadadataSugTokens)

	if (!baseUrl || !tokens.length || !query) return []

	const cacheKey = `sug|${query.trim().toLowerCase()}`
	const cached = getCache<string[]>(cacheKey)
	if (cached !== CACHE_MISS) return cached

	// Round‑robin по токенам, чтобы лучше распределять нагрузку
	const startIndex = lastTokenIndex % tokens.length

	let response: DaDataResponse | null = null
	for (let i = 0; i < tokens.length; i++) {
		const token = tokens[(startIndex + i) % tokens.length]
		response = await callSugEndpoint(`${baseUrl}address`, token, {
			query,
			count: 10,
			locations: [{ kladr_id: '77' }, { kladr_id: '50' }],
			locations_boost: [{ kladr_id: '77' }],
			from_bound: { value: 'street' },
			to_bound: { value: 'street' },
			restrict_value: true
		})
		lastTokenIndex = (startIndex + i + 1) % tokens.length
		if (response && response.suggestions?.length) break
	}

	// Гранулярный запрос по улицам не понимает ввод «от посёлка»
	// («СНТ Лесная поляна, Вишнёвая») — тогда пробуем свободный поиск
	// без границ и берём только предложения с заполненной улицей.
	if (!response?.suggestions?.length) {
		for (let i = 0; i < tokens.length; i++) {
			const token = tokens[(lastTokenIndex + i) % tokens.length]
			response = await callSugEndpoint(`${baseUrl}address`, token, {
				query,
				count: 10,
				locations: [{ kladr_id: '77' }, { kladr_id: '50' }],
				locations_boost: [{ kladr_id: '77' }]
			})
			lastTokenIndex = (lastTokenIndex + i + 1) % tokens.length
			if (response?.suggestions?.length) break
		}
		if (response?.suggestions) {
			response = { suggestions: response.suggestions.filter(s => s.data?.street) }
		}
	}

	if (!response?.suggestions?.length) {
		setCache(cacheKey, [])
		return []
	}

	const streets = response.suggestions.map(i => formatStreetSuggestion(i.data))

	const uniq = Array.from(new Set(streets))
	setCache(cacheKey, uniq)
	return uniq
}

/**
 * Подпись подсказки улицы. Для Москвы — просто улица. Вне Москвы в скобках
 * перечисляется контекст ДО РАЙОНА ОБЛАСТИ включительно: одноимённые СНТ,
 * сёла и посёлки в разных районах МО должны различаться прямо в подсказке,
 * иначе оператор выберет не тот и вызов уедет не туда.
 */
export function formatStreetSuggestion(d: any): string {
	let result = `${d.street} ${d.street_type_full}`
	const isMoscowCity = d.city === 'Москва' || d.region === 'Москва'
	const parts: string[] = []
	if (d.city && d.city !== 'Москва') parts.push(d.city)
	if (d.settlement) parts.push(`${d.settlement_type_full || d.settlement_type || ''} ${d.settlement}`.trim())
	if (!isMoscowCity && d.area_with_type) parts.push(d.area_with_type)
	if (parts.length) result += ` (${parts.join(', ')})`
	return result
}

export async function resolveFullAddress(query: string): Promise<{
	a_reg: number
	a_dis: string
	a_str: string
	a_sa: string
	a_h: string
	a_d: [string | null, string, string, string]
} | null> {
	const config = useRuntimeConfig()
	const baseUrl = config.dadadataSugUrl
	const tokens = getSugTokens(config.dadadataSugTokens)

	if (!baseUrl || !tokens.length || !query) return null

	// Подпись из подсказки несёт контекст в скобках: «Вишнёвая улица
	// (СНТ Лесная поляна, Раменский р-н)». DaData надёжнее разбирает его
	// в порядке «область → район → посёлок → улица», чем в скобках.
	let normQuery = query.trim()
	const paren = normQuery.match(/^(.*?)\s*\(([^)]+)\)\s*$/)
	if (paren) {
		normQuery = `Московская обл, ${paren[2].trim()}, ${paren[1].trim()}`
	}
	const cacheKey = `full|${normQuery.toLowerCase()}`
	const cached = getCache<{
		a_reg: number
		a_dis: string
		a_str: string
		a_sa: string
		a_h: string
		a_d: [string | null, string, string, string]
	} | null>(cacheKey)
	if (cached !== CACHE_MISS) return cached

	const startIndex = lastTokenIndex % tokens.length
	let data: any | null = null

	for (let i = 0; i < tokens.length; i++) {
		const token = tokens[(startIndex + i) % tokens.length]
		const first = await callSugEndpoint(`${baseUrl}address`, token, {
			query: normQuery,
			locations: [{ kladr_id: '77' }, { kladr_id: '50' }]
		})
		if (!first?.suggestions?.[0]?.unrestricted_value) continue

		const second = await callSugEndpoint(`${baseUrl}address`, token, {
			query: first.suggestions[0].unrestricted_value,
			locations: [{ kladr_id: '77' }, { kladr_id: '50' }],
			count: 1
		})

		data = second?.suggestions?.[0]?.data || null
		lastTokenIndex = (startIndex + i + 1) % tokens.length
		if (data) break
	}

	if (!data) {
		// Фоллбек на платный DaData, если настроен
		const paid = await callPaidEndpoint(
			config.dadadataUrl,
			config.dadadataToken,
			config.dadadataSecret,
			normQuery
		)
		if (!paid) {
			setCache(cacheKey, null)
			return null
		}
		data = paid
	}

	const d = data
	let sa = d?.city === 'Москва' ? '' : `г. ${d?.city}`
	if (d?.settlement) sa += `, ${d?.settlement_type}. ${d?.settlement}`
	if (d?.street) sa += `, ${d?.street} ${d?.street_type_full}`

	const result = {
		a_reg: parseInt(String(d?.kladr_id || '').substring(0, 2) || '0', 10),
		// Москва — район города; область — район области (area): для платных
		// вызовов вне Москвы важна точность именно до района МО.
		a_dis: d?.city_district || d?.area || d?.city || '',
		// Вне Москвы улица хранится ВМЕСТЕ с контекстом «(посёлок, район)» —
		// тем же, что показан в подсказке: иначе одноимённые улицы разных
		// посёлков склеиваются в один адрес.
		a_str: d?.street ? formatStreetSuggestion(d) : '',
		a_sa: sa,
		a_h: d?.house ? `${d.house}${d.block_type || ''}${d.block || ''}` : '',
		a_d: [d?.flat || null, '', '', '']
	} as const

	setCache(cacheKey, result)
	return result
}

/**
 * Get coordinates for a Moscow address via DaData suggest.
 * Returns { lat, lon, city_district } or null.
 */
export async function getAddressCoords(
	str: string,
	h?: string
): Promise<{ lat: number; lon: number; city_district: string } | null> {
	const config = useRuntimeConfig()
	const baseUrl = config.dadadataSugUrl
	const tokens = getSugTokens(config.dadadataSugTokens)

	if (!baseUrl || !tokens.length || !str) return null

	const query = h ? `г Москва, ${str}, д ${h}` : `г Москва, ${str}`
	const cacheKey = `coords|${query.trim().toLowerCase()}`
	const cached = getCache<{ lat: number; lon: number; city_district: string } | null>(cacheKey)
	if (cached !== CACHE_MISS) return cached

	const startIndex = lastTokenIndex % tokens.length

	for (let i = 0; i < tokens.length; i++) {
		const token = tokens[(startIndex + i) % tokens.length]
		const response = await callSugEndpoint(`${baseUrl}address`, token, {
			query,
			count: 1,
			locations: [{ kladr_id: '77' }]
		})
		lastTokenIndex = (startIndex + i + 1) % tokens.length

		const data = response?.suggestions?.[0]?.data
		if (data?.geo_lat && data?.geo_lon) {
			const result = {
				lat: parseFloat(data.geo_lat),
				lon: parseFloat(data.geo_lon),
				city_district: data.city_district || ''
			}
			setCache(cacheKey, result)
			return result
		}
	}

	setCache(cacheKey, null)
	return null
}

/**
 * Normalization map: OSM/colloquial names → official Moscow district names.
 * Nominatim sometimes returns informal names or alternate forms.
 */
const DISTRICT_ALIASES: Record<string, string> = {
	Бауманка: 'Басманный',
	Бауманский: 'Басманный',
	Канатчиково: 'Донской',
	'Нагатинская пойма': 'Нагатинский Затон',
	Чертаново: 'Чертаново Северное'
}

/**
 * Normalize raw Nominatim district string to official Moscow district name.
 * Strips "район"/"р-н" prefix/suffix and applies alias corrections.
 */
function normalizeDistrictName(raw: string): string {
	const clean = raw
		.replace(/^(район|р-н)\s+/i, '')
		.replace(/\s+(район|р-н)$/i, '')
		.trim()
	return DISTRICT_ALIASES[clean] || clean
}

/**
 * Query Nominatim with a single search string.
 * Returns normalized district name or null.
 */
async function nominatimSearch(searchQuery: string): Promise<string | null> {
	const url = `https://nominatim.openstreetmap.org/search?q=${encodeURIComponent(searchQuery)}&format=json&addressdetails=1&limit=1&accept-language=ru`
	const resp = await ofetch<any[]>(url, {
		headers: { 'User-Agent': NOMINATIM_USER_AGENT },
		timeout: NOMINATIM_TIMEOUT_MS,
		retry: 1,
		retryDelay: 1100 // Nominatim rate limit: 1 req/sec
	})
	const addr = resp?.[0]?.address
	if (!addr) return null

	const raw = addr.suburb || addr.city_district || ''
	if (!raw) return null

	return normalizeDistrictName(raw) || null
}

/**
 * Resolve Moscow district via OSM Nominatim forward geocoding.
 *
 * Strategy (in order of reliability):
 * 1. Nominatim search with full street type + house + Москва
 * 2. Nominatim search without house (street-level fallback)
 * 3. DaData city_district (last resort)
 *
 * Results are cached with 3-min TTL.
 */
export async function resolveDistrictDaData(str: string, h?: string): Promise<string | null> {
	if (!str || str.length < 2) return null

	const cacheKey = `dis|${str.toLowerCase()}|${(h || '').toLowerCase()}`
	const cached = getCache<string | null>(cacheKey)
	if (cached !== CACHE_MISS) return cached

	// Step 1: Nominatim with street + house (most precise)
	if (h) {
		try {
			const result = await nominatimSearch(`${str}, ${h}, Москва, Россия`)
			if (result) {
				setCache(cacheKey, result)
				return result
			}
		} catch (_) {
			/* fall through to next strategy */
		}
	}

	// Step 2: Nominatim with street only (broader match)
	try {
		const result = await nominatimSearch(`${str}, Москва, Россия`)
		if (result) {
			setCache(cacheKey, result)
			return result
		}
	} catch (_) {
		/* fall through to DaData fallback */
	}

	// Step 3: DaData city_district (last resort — less reliable but better than nothing)
	try {
		const coords = await getAddressCoords(str, h)
		if (coords?.city_district) {
			const normalized = normalizeDistrictName(coords.city_district)
			if (normalized) {
				setCache(cacheKey, normalized)
				return normalized
			}
		}
	} catch (_) {
		/* non-fatal */
	}

	// Cache negative result to avoid repeated lookups
	setCache(cacheKey, null)
	return null
}
