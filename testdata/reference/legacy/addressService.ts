/**
 * Address Service — центральный сервис подсистемы адресов.
 *
 * Обязанности:
 * 1. Подсказки улиц (local-first, DaData fallback)
 * 2. Валидация адреса (проверка опечаток, предложение исправлений)
 * 3. getOrCreate: поиск/создание адреса в БД с дедупликацией и обогащением
 * 4. Обучение: кэширование ответов DaData в локальный справочник
 *
 * Принципы:
 * - НИКОГДА не исправлять адрес автоматически. Только предлагать.
 * - Пользователь может принудительно пропустить все проверки.
 * - При создании нового адреса — обогащать из pnds (район → ПНД).
 * - Максимум данных из локальных источников, минимум обращений к DaData.
 */

import { createError } from 'h3'
import { getStreetDictionary } from './streetDictionary'
import { getStreetSuggestions, resolveDistrictDaData } from '../services/dadadata'
import { isLikelyWrongLayout, latinToCyrillic } from '../utils/keyboardLayout'
import { hasLatin } from '../utils/fuzzyMatch'
import { inferFromNeighbors } from './addressNeighbors'
import type { NeighborCandidate, NeighborInference } from './addressNeighbors'

/* ── Types ── */

export interface AddressSuggestion {
	/** Full street name: "Тверская улица" */
	fullName: string
	/** Source: 'local' or 'dadata' */
	source: 'local' | 'dadata'
	/** Region: 77 or 50 */
	region?: number
	/** Settlement for MO */
	settlement?: string
	/** Match quality 0-100 */
	score: number
}

export interface ValidationResult {
	/** Is the address valid? */
	status: 'valid' | 'warning' | 'unknown'
	/** Validation message */
	message: string
	/** Suggested correction (if any) */
	correction?: {
		str: string
		confidence: number
		reason: string
	}
	/** Was the street found in dictionary? */
	knownStreet: boolean
}

export interface ResolvedAddress {
	/** Document _key */
	_key: string
	/** Street name */
	str: string
	/** House number */
	h: string
	/** Region code */
	reg?: number
	/** District */
	dis?: string
	/** PND number (from pnds collection) */
	pnd?: number | null
	/** Whether this is a newly created address */
	isNew: boolean
}

/* ── House number normalization (canonical form) ── */

export function formatHouseCanonical(raw: string): string {
	if (!raw) return raw
	let h = raw.trim()
	if (!h) return h

	h = h.replace(/\s+/g, '')
	h = h.replace(/корпус\.?/gi, 'к')
	h = h.replace(/корп\.?/gi, 'к')
	h = h.replace(/строение\.?/gi, 'с')
	h = h.replace(/строен\.?/gi, 'с')
	h = h.replace(/стр\.?/gi, 'с')
	h = h.replace(/владение\.?/gi, 'вл')
	h = h.replace(/влад\.?/gi, 'вл')
	h = h.replace(/\./g, '')

	let result = ''
	let i = 0
	while (i < h.length) {
		const ch = h[i]
		const lower = ch.toLowerCase()
		if (i + 1 < h.length && lower === 'в' && h[i + 1].toLowerCase() === 'л') {
			result += 'вл'
			i += 2
			continue
		}
		if (lower === 'к' || lower === 'с') {
			const next = i + 1 < h.length ? h[i + 1] : ''
			result += next && (/\d/.test(next) || /[а-яёА-ЯЁ]/.test(next)) ? lower : ch.toUpperCase()
			i++
			continue
		}
		if (/[а-яёА-ЯЁ]/.test(ch)) {
			result += ch.toUpperCase()
			i++
			continue
		}
		result += ch
		i++
	}
	return result
}

/* ── Street normalization for comparison ── */

export const STREET_TYPE_RE = [
	/\bул(ица)?\.?\s*/gi,
	/\bулица\s*/gi,
	/\bпр(оспект)?[-.]?(кт)?\.?\s*/gi,
	/\bпер(еулок)?\.?\s*/gi,
	/\bб[-.]?р\.?\s*/gi,
	/\bбульвар\s*/gi,
	/\bпл(ощадь)?\.?\s*/gi,
	/\bш(оссе)?\.?\s*/gi,
	/\bнаб(ережная)?\.?\s*/gi,
	/\bпроезд\.?\s*/gi,
	/\bтупик\.?\s*/gi,
	/\bаллея\.?\s*/gi
]

export function normalizeStreetForCompare(str: string): string {
	if (!str) return ''
	let s = str.toLowerCase()
	for (const re of STREET_TYPE_RE) s = s.replace(re, '')
	return s
		.replace(/\(.*?\)/g, '')
		.replace(/[.,\-\s]+/g, ' ')
		.trim()
}

/* ── Lazy helpers ── */

let _dictSingleton: ReturnType<typeof getStreetDictionary> | null = null

async function getDictionary() {
	if (_dictSingleton) return _dictSingleton
	const pg = await import('../db/postgres')
	const pool = pg.getPostgresPool()
	_dictSingleton = getStreetDictionary(
		async (op: string, bv: Record<string, any>) => {
			if (op === 'SELECT_ADDRESSES') {
				const { rows } = await pool.query(
					`SELECT DISTINCT street AS str, region AS reg, settlement FROM addresses WHERE street IS NOT NULL AND street != ''`
				)
				return rows
			}
			if (op === 'SELECT_STREET_DICTIONARY') {
				const { rows } = await pool.query(
					`SELECT full_name AS "fullName", id AS "_key" FROM street_dictionary`
				)
				return rows
			}
			if (op === 'UPSERT_STREET') {
				await pool.query(
					`INSERT INTO street_dictionary (full_name) VALUES ($1)
				 ON CONFLICT (full_name) DO NOTHING`,
					[bv.fullName || '']
				)
				return []
			}
			return []
		},
		async () => {
			await pool.query(`CREATE TABLE IF NOT EXISTS street_dictionary (
			id serial PRIMARY KEY,
			full_name text UNIQUE NOT NULL,
			created_at timestamptz DEFAULT now()
		)`)
		}
	)
	return _dictSingleton
}

/* ── Service Class ── */

export class AddressService {
	/**
	 * Get street suggestions for autocomplete.
	 * Local dictionary first (instant), DaData as fallback (async).
	 *
	 * Returns merged + deduplicated results, sorted by relevance.
	 */
	async suggest(query: string, limit: number = 10): Promise<AddressSuggestion[]> {
		if (!query || query.length < 2) return []

		const dict = await getDictionary()

		// 1. Local search (fast, ~1ms)
		const localResults = await dict.search(query, limit)
		const suggestions: AddressSuggestion[] = localResults.map(r => ({
			fullName: r.entry.fullName,
			source: 'local' as const,
			region: r.entry.region,
			settlement: r.entry.settlement,
			score: r.score
		}))

		// 2. If local results are insufficient, query DaData
		if (suggestions.length < limit && query.length >= 3) {
			try {
				const dadataResults = await getStreetSuggestions(query)
				if (dadataResults.length > 0) {
					// Learn from DaData in background (fire & forget)
					dict
						.learnFromDaData(dadataResults)
						.catch(e => console.warn('[AddressService] learnFromDaData failed:', e))

					// Merge with local results, avoiding duplicates
					const localNames = new Set(suggestions.map(s => s.fullName.toLowerCase()))
					for (const name of dadataResults) {
						if (localNames.has(name.toLowerCase())) continue
						suggestions.push({
							fullName: name,
							source: 'dadata',
							score: 50 // DaData results have moderate score
						})
						if (suggestions.length >= limit * 2) break
					}
				}
			} catch (e) {
				// DaData failure is non-fatal — we have local results
				console.warn('[AddressService] DaData suggest failed (using local only):', e)
			}
		}

		// Sort by score descending
		suggestions.sort((a, b) => b.score - a.score)
		return suggestions.slice(0, limit)
	}

	/**
	 * Validate an address (street + house).
	 *
	 * NEVER auto-corrects. Returns validation status and optional correction suggestion.
	 *
	 * Flow:
	 * 1. Check if street exists in dictionary → 'valid'
	 * 2. Check for close match (typo) → 'warning' + correction suggestion
	 * 3. Check correction model (from merge history) → 'warning' + suggestion
	 * 4. No match found → 'unknown' (not necessarily wrong, just unverified)
	 */
	async validate(str: string, h?: string): Promise<ValidationResult> {
		if (!str) {
			return { status: 'warning', message: 'Улица не указана', knownStreet: false }
		}

		// 0a. Keyboard layout check (Latin → Cyrillic)
		if (isLikelyWrongLayout(str)) {
			const corrected = latinToCyrillic(str)
			return {
				status: 'warning',
				message: 'Текст введён в латинской раскладке',
				correction: {
					str: corrected,
					confidence: 95,
					reason: 'Автоматическая смена раскладки QWERTY → ЙЦУКЕН'
				},
				knownStreet: false
			}
		}

		// 0b. Mixed script check
		if (hasLatin(str)) {
			return {
				status: 'warning',
				message: 'Название улицы содержит латинские символы',
				knownStreet: false
			}
		}

		const dict = await getDictionary()

		// 1. Exact match in dictionary
		const exact = await dict.findExact(str)
		if (exact) {
			// Street is known — validate house number format
			const houseMessage = h ? this._validateHouseFormat(h) : null
			return {
				status: houseMessage ? 'warning' : 'valid',
				message: houseMessage || 'Адрес найден в справочнике',
				knownStreet: true
			}
		}

		// 2. Fuzzy match in dictionary (typo detection)
		const closest = await dict.findClosest(str, 0.65)
		if (closest && closest.similarity >= 0.65) {
			const confidence = Math.round(closest.similarity * 100)
			return {
				status: 'warning',
				message: `Улица не найдена. Возможно: «${closest.entry.fullName}»?`,
				correction: {
					str: closest.entry.fullName,
					confidence,
					reason: `Похожее название (${confidence}% совпадение)`
				},
				knownStreet: false
			}
		}

		// 3. Check existing addresses in DB (maybe the street exists but not in dictionary)
		const dbMatch = await this._findInAddresses(str)
		if (dbMatch) {
			return {
				status: 'valid',
				message: 'Адрес найден в базе данных',
				knownStreet: true
			}
		}

		// 4. Unknown street
		return {
			status: 'unknown',
			message: 'Улица не найдена в справочнике. Проверьте написание.',
			knownStreet: false
		}
	}

	/**
	 * Find or create an address in the database.
	 *
	 * Algorithm:
	 * 1. Normalize house number to canonical form
	 * 2. Search for existing address with same (str, h)
	 * 3. If found — return it (optionally enrich with missing data)
	 * 4. If not found — search by normalized street + house (catch minor variants)
	 * 5. If still not found — create new, enrich with PND/district data
	 *
	 * @param skipValidation - if true, skip all corrections and create as-is
	 */
	async getOrCreate(params: {
		str: string
		h: string
		reg?: number
		dis?: string
		pnd?: number | null
		skipValidation?: boolean
	}): Promise<ResolvedAddress> {
		const { str, reg = 77, dis, pnd } = params
		const h = formatHouseCanonical(params.h || '')

		if (!str) {
			throw createError({ statusCode: 400, message: 'Не указана улица' })
		}

		// 1. Exact match (str + h)
		const exactMatch = await this._findExactAddress(str, h)
		if (exactMatch) {
			// Enrich if we have more data than what's stored
			const enriched = await this._enrichExistingAddress(exactMatch, { reg, dis, pnd })
			return { ...enriched, isNew: false }
		}

		// 2. Normalized match (catch case/spacing variants)
		if (!params.skipValidation) {
			const normMatch = await this._findNormalizedAddress(str, h)
			if (normMatch) {
				const enriched = await this._enrichExistingAddress(normMatch, { reg, dis, pnd })
				return { ...enriched, isNew: false }
			}
		}

		// 3. Create new address with enrichment
		const enrichment = await this._getEnrichmentData(str, dis, reg, h)

		const data: Record<string, any> = {
			str,
			h,
			reg: reg || 77,
			dis: dis || enrichment.dis || '',
			// pnd — только явно переданное пользователем значение. Автоматическая
			// подсказка по району (enrichment.pnd) сюда не подмешивается: ПНД —
			// поле, которое должен устанавливать только пользователь.
			pnd: pnd ?? null
		}

		// Additional enrichment fields
		if (enrichment.set) data.set = enrichment.set

		try {
			const { getPostgresPool } = await import('../db/postgres')
			const pool = getPostgresPool()
			const result = await pool.query(
				`INSERT INTO addresses (id, street, house, region, district, pnd_id)
				 VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5::smallint)
				 ON CONFLICT (street, house) WHERE street IS NOT NULL
				 DO UPDATE SET
				   region = CASE WHEN EXCLUDED.region IS NOT NULL AND EXCLUDED.region != '' THEN EXCLUDED.region ELSE addresses.region END,
				   district = CASE WHEN EXCLUDED.district IS NOT NULL AND EXCLUDED.district != '' THEN EXCLUDED.district ELSE addresses.district END,
				   pnd_id = CASE WHEN EXCLUDED.pnd_id IS NOT NULL AND EXCLUDED.pnd_id != 0 THEN EXCLUDED.pnd_id ELSE addresses.pnd_id END
				 RETURNING *, id AS "_key"`,
				[str, h, data.reg || null, data.dis || null, data.pnd != null ? Number(data.pnd) : null]
			)
			const row = result.rows[0]
			return {
				_key: row._key || row.id,
				str: row.street,
				h: row.house,
				reg: row.region,
				dis: row.district,
				pnd: row.pnd_id,
				isNew: true
			}
		} catch (e) {
			console.warn('[AddressService] UPSERT failed:', e)
			throw new Error(`Ошибка при создании адреса: ${e}`)
		}
	}

	/* ── Private helpers ── */

	private _validateHouseFormat(h: string): string | null {
		if (!h) return null
		// Check for obviously wrong patterns
		if (/^\d{5,}$/.test(h)) return 'Номер дома выглядит слишком длинным'
		if (/^0+$/.test(h)) return 'Номер дома не может быть 0'
		if (!/\d/.test(h)) return 'Номер дома должен содержать цифры'
		return null
	}

	private async _findInAddresses(str: string): Promise<boolean> {
		try {
			const { getPostgresPool } = await import('../db/postgres')
			const pool = getPostgresPool()
			const { rows } = await pool.query(
				`SELECT 1 FROM addresses WHERE LOWER(street) = LOWER($1) LIMIT 1`,
				[str]
			)
			return rows.length > 0
		} catch {
			return false
		}
	}

	private async _findExactAddress(str: string, h: string): Promise<any | null> {
		try {
			const { getPostgresPool } = await import('../db/postgres')
			const pool = getPostgresPool()
			const { rows } = await pool.query(
				`SELECT *, id AS "_key" FROM addresses WHERE street = $1 AND house = $2 LIMIT 1`,
				[str, h]
			)
			return rows[0] || null
		} catch {
			return null
		}
	}

	private async _findNormalizedAddress(str: string, h: string): Promise<any | null> {
		const normStr = normalizeStreetForCompare(str)
		const normH = h.toLowerCase()
		if (!normStr) return null

		try {
			const { getPostgresPool } = await import('../db/postgres')
			const pool = getPostgresPool()
			const { rows } = await pool.query(
				`SELECT *, id AS "_key" FROM addresses
				 WHERE street IS NOT NULL AND street != ''
				   -- Число дома лишь СУЖАЕТ кандидатов («10к1», «10 к 1» → 10);
				   -- окончательное решение — по полному каноническому номеру ниже.
				   AND house_num(house) = house_num($1)
				   -- Ключ улицы: без типа («ул.»/«улица») и без учёта порядка
				   -- слов — в базе есть и «Саляма Адиля улица», и обратный
				   -- порядок.
				   AND street_key(street) % street_key($2)
				 ORDER BY similarity(street_key(street), street_key($2)) DESC
				 LIMIT 5`,
				[normH, str.toLowerCase()]
			)
			// Совпадением считается только ПОЛНОЕ равенство канонического номера
			// дома. Раньше хватало равенства ведущего числа, и «7к7» молча
			// приклеивался к существующему «7к1» на той же улице — карта
			// получала чужой дом (карта 2610837). Ввод пользователя неприкосновенен:
			// «7к7» и «7к1» — разные дома, «7 к 7» и «7к7» — один.
			for (const row of rows) {
				if (
					normalizeStreetForCompare(row.street) === normStr &&
					formatHouseCanonical(row.house || '').toLowerCase() === normH
				)
					return row
			}
			return null
		} catch (e) {
			console.warn('[AddressService] Normalized search failed:', e)
			return null
		}
	}

	/**
	 * Enrich an existing address with additional data.
	 * Only updates fields that are currently empty/null.
	 */
	private async _enrichExistingAddress(
		addr: any,
		params: { reg?: number; dis?: string; pnd?: number | null }
	): Promise<ResolvedAddress> {
		const updates: Record<string, any> = {}

		// Only fill in missing data, never overwrite
		if (params.reg && !addr.region) updates.region = params.reg
		if (params.dis && !addr.district) updates.district = params.dis
		if (params.pnd != null && (addr.pnd_id == null || addr.pnd_id === 0))
			updates.pnd_id = params.pnd

		// Район подтягиваем автоматически (справочная информация), но ПНД —
		// только если явно передан пользователем (params.pnd выше). Автоматическая
		// подсказка ПНД по району сюда не подмешивается.
		if (!addr.district) {
			const enrichment = await this._getEnrichmentData(
				addr.street,
				params.dis,
				addr.region || params.reg,
				addr.house || ''
			)
			if (enrichment.dis && !updates.district) updates.district = enrichment.dis
		}

		if (Object.keys(updates).length > 0) {
			try {
				const { getPostgresPool } = await import('../db/postgres')
				const pool = getPostgresPool()
				const setClauses = Object.keys(updates).map((k, i) => `"${k}" = $${i + 2}`)
				await pool.query(`UPDATE addresses SET ${setClauses.join(', ')} WHERE id = $1`, [
					addr._key,
					...Object.values(updates)
				])
			} catch (e) {
				console.warn('[AddressService] Enrichment update failed:', e)
			}
		}

		return {
			_key: addr._key,
			str: addr.street,
			h: addr.house,
			reg: updates.region || addr.region,
			dis: updates.district || addr.district,
			pnd: updates.pnd_id ?? addr.pnd_id,
			isNew: false
		}
	}

	/**
	 * Get enrichment data for an address:
	 * - District via Nominatim geocoding (most reliable)
	 * - PND number from pnds collection
	 *
	 * Used when creating a new address record.
	 */
	private async _getEnrichmentData(
		str: string,
		dis?: string,
		_reg?: number,
		h?: string
	): Promise<{ dis?: string; pnd?: number | null; set?: string }> {
		const result: { dis?: string; pnd?: number | null; set?: string } = {}

		// Район: сначала консенсус соседних домов (локально), затем Nominatim
		if (!dis && str) {
			if (h) {
				const nb = await this._neighborInference(str, h)
				if (nb && nb.confidence === 'high') result.dis = nb.dis
			}
			if (!result.dis) {
				try {
					const district = await resolveDistrictDaData(str, h || undefined)
					if (district) result.dis = district
				} catch (_) {
					/* non-fatal */
				}
			}
		}

		const district = dis || result.dis
		if (!district) return result

		// Resolve PND from district name
		const pndResult = await this._resolveDistrictPnd(district)
		if (pndResult.pnd) result.pnd = pndResult.pnd
		if (pndResult.matchedDis && !dis) result.dis = pndResult.matchedDis

		return result
	}

	/**
	 * Resolve district and PND for a street+house combination.
	 * Used for real-time frontend lookups (before call is saved).
	 *
	 * Порядок источников:
	 * 1. Соседние дома той же улицы (локально, мгновенно). Автоподстановка —
	 *    только при выраженном консенсусе (см. addressNeighbors.ts): один
	 *    исторически кривой район не пройдёт порог, нужно согласие соседей.
	 * 2. Внешний геокодер (Nominatim → DaData) — когда соседи молчат или спорят.
	 * 3. Спорные кандидаты соседей возвращаются списком candidates — интерфейс
	 *    предлагает выбор, ничего не подставляя молча.
	 */
	async resolveDistrict(
		str: string,
		h: string
	): Promise<{
		dis: string
		pnd: number | null
		source: 'neighbors' | 'geo' | 'none'
		candidates: NeighborCandidate[]
	}> {
		if (!str || str.length < 2) return { dis: '', pnd: null, source: 'none', candidates: [] }

		// 1. Соседи той же улицы
		const nb = await this._neighborInference(str, h)
		if (nb && nb.confidence === 'high') {
			const pnd = nb.pnd ?? (await this._resolveDistrictPnd(nb.dis)).pnd
			return { dis: nb.dis, pnd, source: 'neighbors', candidates: nb.candidates }
		}

		// 2. Внешний геокодер
		let dis = ''
		try {
			const district = await resolveDistrictDaData(str, h || undefined)
			if (district) dis = district
		} catch (_) {
			/* non-fatal */
		}

		if (dis) {
			const pndResult = await this._resolveDistrictPnd(dis)
			return {
				dis: pndResult.matchedDis || dis,
				pnd: pndResult.pnd,
				source: 'geo',
				candidates: []
			}
		}

		// 3. Геокодер молчит — отдать спорных кандидатов соседей на выбор
		if (nb) {
			const candidates = await Promise.all(
				nb.candidates.map(async c => ({
					...c,
					pnd: c.pnd ?? (await this._resolveDistrictPnd(c.dis)).pnd
				}))
			)
			return { dis: '', pnd: null, source: 'none', candidates }
		}

		return { dis: '', pnd: null, source: 'none', candidates: [] }
	}

	/**
	 * Соседи по улице: адреса с тем же street_key и известным районом.
	 * Оператор % задействует триграммный GIN, равенство ключа — точный отбор.
	 */
	private async _neighborInference(str: string, h: string): Promise<NeighborInference | null> {
		if (!str || !h) return null
		try {
			const { getPostgresPool } = await import('../db/postgres')
			const pool = getPostgresPool()
			const { rows } = await pool.query(
				`SELECT house, district, pnd_id FROM addresses
				 WHERE street IS NOT NULL AND street != ''
				   AND district IS NOT NULL AND district != ''
				   AND street_key(street) % street_key($1)
				   AND street_key(street) = street_key($1)
				 LIMIT 400`,
				[str.toLowerCase()]
			)
			return inferFromNeighbors(h, rows)
		} catch (e) {
			console.warn('[AddressService] neighbor inference failed:', e)
			return null
		}
	}

	/**
	 * Look up PND number by district name.
	 * Matching strategy (in order):
	 * 1. Exact match
	 * 2. Case-insensitive match
	 * 3. ё/е-insensitive match (Nominatim returns ё, DB may have е)
	 */
	private async _resolveDistrictPnd(
		dis: string
	): Promise<{ pnd: number | null; matchedDis: string }> {
		if (!dis) return { pnd: null, matchedDis: '' }

		// Normalize ё→е for fallback comparison
		const disNoYo = dis.replace(/ё/gi, 'е')

		try {
			const { getPostgresPool } = await import('../db/postgres')
			const pool = getPostgresPool()
			const { rows } = await pool.query(
				`SELECT pnd, dis FROM pnds
				 WHERE dis = $1
				    OR LOWER(dis) = LOWER($1)
				    OR LOWER(REPLACE(dis, 'ё', 'е')) = LOWER($2)
				 LIMIT 1`,
				[dis, disNoYo]
			)
			const found = rows[0]
			if (found) {
				return {
					pnd: typeof found.pnd === 'number' ? found.pnd : Number(found.pnd) || null,
					matchedDis: found.dis
				}
			}
		} catch (_) {
			/* non-fatal */
		}

		return { pnd: null, matchedDis: dis }
	}
}

/* ── Singleton ── */

let singleton: AddressService | null = null

export function getAddressService(): AddressService {
	if (!singleton) {
		singleton = new AddressService()
	}
	return singleton
}
