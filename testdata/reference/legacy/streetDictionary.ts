/**
 * Street Dictionary — локальный справочник улиц с быстрым поиском.
 *
 * Источники данных (в порядке приоритета):
 * 1. Статический seed (moscow-streets-seed.ts) — ~600 записей, всегда доступен
 * 2. Таблица `addresses` — все ранее использованные адреса
 * 3. Таблица `street_dictionary` — выученные из DaData/ручного ввода улицы
 *
 * Особенности:
 * - In-memory индекс для sub-ms поиска (prefix + contains + fuzzy)
 * - Автоматическое обучение: каждый валидный ответ DaData кэшируется
 * - Периодическая перезагрузка из БД (5 мин TTL)
 * - Thread-safe загрузка (dedup concurrent loads)
 */

import { SEED, STREET_TYPES } from '../data/moscow-streets-seed'
import { bigramDice, editDistance } from '../utils/fuzzyMatch'

/* ── Types ── */

export interface StreetEntry {
	/** Название улицы без типа: "Тверская" */
	name: string
	/** Тип улицы полностью: "улица" */
	type: string
	/** Полное название: "Тверская улица" */
	fullName: string
	/** Регион: 77 (Москва) или 50 (МО) */
	region: number
	/** Населённый пункт (для МО): "Балашиха" */
	settlement?: string
	/** Источник данных */
	source: 'seed' | 'db' | 'dadata' | 'manual'
}

export interface SearchResult {
	entry: StreetEntry
	score: number
	matchType: 'exact' | 'prefix' | 'contains' | 'fuzzy'
}

/* ── Street type aliases for normalization ── */

const TYPE_ALIASES: Record<string, string> = {
	// Улица
	ул: 'улица',
	'ул.': 'улица',
	улица: 'улица',
	// Переулок
	пер: 'переулок',
	'пер.': 'переулок',
	переулок: 'переулок',
	// Проспект
	пр: 'проспект',
	'пр.': 'проспект',
	просп: 'проспект',
	'пр-кт': 'проспект',
	'пр-т': 'проспект',
	проспект: 'проспект',
	// Шоссе
	ш: 'шоссе',
	'ш.': 'шоссе',
	шоссе: 'шоссе',
	// Бульвар
	бул: 'бульвар',
	'бул.': 'бульвар',
	'б-р': 'бульвар',
	'б-р.': 'бульвар',
	бульвар: 'бульвар',
	// Площадь
	пл: 'площадь',
	'пл.': 'площадь',
	площадь: 'площадь',
	// Набережная
	наб: 'набережная',
	'наб.': 'набережная',
	набережная: 'набережная',
	// Проезд
	'пр-д': 'проезд',
	'пр-зд': 'проезд',
	проезд: 'проезд',
	// Тупик
	туп: 'тупик',
	'туп.': 'тупик',
	тупик: 'тупик',
	// Аллея
	аллея: 'аллея'
}

/* ── Query parsing ── */

interface ParsedQuery {
	/** Name part of the query (without type) */
	name: string
	/** Detected type from query, or null */
	type: string | null
	/** Settlement from parentheses, e.g. "Балашиха" */
	settlement: string | null
	/** Original lowercase query */
	raw: string
}

function parseQuery(query: string): ParsedQuery {
	const raw = query.trim().toLowerCase()
	let name = raw
	let type: string | null = null
	let settlement: string | null = null

	// Extract settlement from parentheses: "ленина ул (балашиха)"
	const parenMatch = name.match(/\(([^)]+)\)\s*$/)
	if (parenMatch) {
		settlement = parenMatch[1].trim()
		name = name.replace(parenMatch[0], '').trim()
	}

	// Try to extract type suffix: "тверская улица" → name="тверская", type="улица"
	const words = name.split(/\s+/)
	if (words.length >= 2) {
		const lastWord = words[words.length - 1]
		const resolvedType = TYPE_ALIASES[lastWord]
		if (resolvedType) {
			type = resolvedType
			name = words.slice(0, -1).join(' ')
		} else {
			// Check first word as type prefix: "ул. тверская"
			const firstWord = words[0].replace(/\.$/, '')
			const resolvedFirst = TYPE_ALIASES[firstWord] || TYPE_ALIASES[firstWord + '.']
			if (resolvedFirst) {
				type = resolvedFirst
				name = words.slice(1).join(' ')
			}
		}
	}

	return { name, type, settlement, raw }
}

/* ── Dictionary Class ── */

export class StreetDictionary {
	/** All entries, keyed by fullName.toLowerCase() for dedup */
	private entries = new Map<string, StreetEntry>()

	/** Prefix index: first 3 chars → entries */
	private prefixIndex = new Map<string, StreetEntry[]>()

	/** Loading state */
	private loaded = false
	private loading: Promise<void> | null = null
	private lastLoad = 0
	private readonly CACHE_TTL = 5 * 60 * 1000 // 5 min

	/* eslint-disable-next-line no-useless-constructor */
	constructor(
		private readonly dbQuery: (query: string, bindVars: Record<string, any>) => Promise<any[]>,
		private readonly ensureCollection: () => Promise<void>
	) {}

	/* ── Loading ── */

	async ensureLoaded(): Promise<void> {
		const stale = Date.now() - this.lastLoad > this.CACHE_TTL
		if (this.loaded && !stale) return
		if (this.loading) return this.loading
		this.loading = this._load()
		await this.loading
		this.loading = null
	}

	private async _load(): Promise<void> {
		try {
			// 1. Load seed
			this._loadSeed()

			// 2. Load from addresses collection (existing data)
			await this._loadFromAddresses()

			// 3. Load from street_dictionary collection (learned streets)
			await this._loadFromDictionary()

			// 4. Rebuild prefix index
			this._buildPrefixIndex()

			this.loaded = true
			this.lastLoad = Date.now()
			console.log(`[StreetDictionary] Loaded ${this.entries.size} streets`)
		} catch (e) {
			console.error('[StreetDictionary] Load failed:', e)
			// Even on failure, seed data is available
			if (this.entries.size > 0) {
				this.loaded = true
				this.lastLoad = Date.now()
			}
		}
	}

	private _loadSeed(): void {
		for (const entry of SEED) {
			const [name, typeIdx, region, settlement] = entry as [string, number, number?, string?]
			const type = STREET_TYPES[typeIdx] || 'улица'
			const fullName = settlement ? `${name} ${type} (${settlement})` : `${name} ${type}`
			this._addEntry({
				name,
				type,
				fullName,
				region: region || 77,
				settlement,
				source: 'seed'
			})
		}
	}

	private async _loadFromAddresses(): Promise<void> {
		try {
			const rows = await this.dbQuery('SELECT_ADDRESSES', {})
			for (const row of rows) {
				const parsed = this._parseStreetString(row.str)
				if (parsed) {
					this._addEntry({ ...parsed, source: 'db' })
				}
			}
		} catch (e) {
			console.warn('[StreetDictionary] Failed to load from addresses:', e)
		}
	}

	private async _loadFromDictionary(): Promise<void> {
		try {
			await this.ensureCollection()
			const rows = await this.dbQuery('SELECT_STREET_DICTIONARY', {})
			for (const row of rows) {
				this._addEntry({
					name: row.name || '',
					type: row.type || 'улица',
					fullName: row.fullName || `${row.name} ${row.type}`,
					region: row.region || 77,
					settlement: row.settlement || undefined,
					source: row.source || 'dadata'
				})
			}
		} catch (e) {
			// Collection may not exist yet — that's fine
			console.warn('[StreetDictionary] Failed to load from dictionary:', e)
		}
	}

	/**
	 * Parse a stored street string like "Тверская улица" or "Ленина улица (Балашиха)"
	 * into a StreetEntry.
	 */
	private _parseStreetString(str: string): Omit<StreetEntry, 'source'> | null {
		if (!str || str.length < 2) return null

		const trimmed = str.trim()

		// Extract settlement
		let settlement: string | undefined
		let main = trimmed
		const parenMatch = main.match(/\(([^)]+)\)\s*$/)
		if (parenMatch) {
			settlement = parenMatch[1].trim()
			main = main.replace(parenMatch[0], '').trim()
		}

		// Try to split into name + type
		const words = main.split(/\s+/)
		if (words.length >= 2) {
			const lastWord = words[words.length - 1].toLowerCase()
			const resolvedType = TYPE_ALIASES[lastWord]
			if (resolvedType) {
				const name = words.slice(0, -1).join(' ')
				const fullName = settlement
					? `${name} ${resolvedType} (${settlement})`
					: `${name} ${resolvedType}`
				return {
					name,
					type: resolvedType,
					fullName,
					region: settlement ? 50 : 77,
					settlement
				}
			}
		}

		// No type found — treat as street name, type = "улица" by default
		return {
			name: main,
			type: 'улица',
			fullName: settlement ? `${main} улица (${settlement})` : `${main} улица`,
			region: settlement ? 50 : 77,
			settlement
		}
	}

	private _addEntry(entry: StreetEntry): void {
		const key = entry.fullName.toLowerCase()
		// Don't overwrite seed/db entries with lower-priority sources
		const existing = this.entries.get(key)
		if (existing) {
			const priority = { seed: 0, db: 1, dadata: 2, manual: 3 }
			if (priority[entry.source] <= priority[existing.source]) return
		}
		this.entries.set(key, entry)
	}

	private _buildPrefixIndex(): void {
		this.prefixIndex.clear()
		for (const entry of this.entries.values()) {
			const nameKey = entry.name.toLowerCase()
			if (nameKey.length < 2) continue

			// Index by first 2 and 3 chars of name
			for (const len of [2, 3]) {
				if (nameKey.length >= len) {
					const prefix = nameKey.substring(0, len)
					if (!this.prefixIndex.has(prefix)) this.prefixIndex.set(prefix, [])
					this.prefixIndex.get(prefix)!.push(entry)
				}
			}
		}
	}

	/* ── Search ── */

	/**
	 * Search streets by query.
	 * Returns up to `limit` results, sorted by relevance.
	 */
	async search(query: string, limit: number = 10): Promise<SearchResult[]> {
		await this.ensureLoaded()

		if (!query || query.length < 2) return []

		const parsed = parseQuery(query)
		const results: SearchResult[] = []
		const seen = new Set<string>()

		function addResult(entry: StreetEntry, score: number, matchType: SearchResult['matchType']) {
			const key = entry.fullName.toLowerCase()
			if (seen.has(key)) return
			seen.add(key)
			results.push({ entry, score, matchType })
		}

		const nameQuery = parsed.name

		// 1. Exact name match
		for (const entry of this.entries.values()) {
			const entryNameLow = entry.name.toLowerCase()
			if (entryNameLow === nameQuery) {
				const typeBonus = parsed.type && entry.type === parsed.type ? 20 : 0
				const settlementBonus =
					parsed.settlement && entry.settlement?.toLowerCase() === parsed.settlement ? 15 : 0
				addResult(entry, 100 + typeBonus + settlementBonus, 'exact')
			}
		}

		// 2. Prefix match using index
		const prefixLen = Math.min(nameQuery.length, 3)
		if (prefixLen >= 2) {
			const prefix = nameQuery.substring(0, prefixLen)
			const candidates = this.prefixIndex.get(prefix) || []
			for (const entry of candidates) {
				const entryNameLow = entry.name.toLowerCase()
				if (entryNameLow.startsWith(nameQuery)) {
					const lengthBonus = Math.max(0, 20 - (entryNameLow.length - nameQuery.length))
					const typeBonus = parsed.type && entry.type === parsed.type ? 15 : 0
					const settlementBonus =
						parsed.settlement && entry.settlement?.toLowerCase() === parsed.settlement ? 10 : 0
					addResult(entry, 80 + lengthBonus + typeBonus + settlementBonus, 'prefix')
				}
			}
		}

		// 3. Contains match (for longer queries, min 3 chars)
		if (nameQuery.length >= 3) {
			for (const entry of this.entries.values()) {
				const entryNameLow = entry.name.toLowerCase()
				if (entryNameLow.includes(nameQuery) && !entryNameLow.startsWith(nameQuery)) {
					const typeBonus = parsed.type && entry.type === parsed.type ? 10 : 0
					addResult(entry, 60 + typeBonus, 'contains')
				}
			}
		}

		// 4. Fuzzy match (for queries >= 4 chars, only if not enough results)
		if (nameQuery.length >= 4 && results.length < limit) {
			const needed = limit - results.length
			const fuzzyResults: Array<{ entry: StreetEntry; sim: number }> = []

			for (const entry of this.entries.values()) {
				const entryNameLow = entry.name.toLowerCase()
				if (seen.has(entry.fullName.toLowerCase())) continue

				// Quick length filter
				if (Math.abs(entryNameLow.length - nameQuery.length) > 4) continue

				// Try edit distance first (faster for short strings)
				const dist = editDistance(nameQuery, entryNameLow)
				const maxLen = Math.max(nameQuery.length, entryNameLow.length)
				const ratio = 1 - dist / maxLen

				if (dist <= 2 && ratio >= 0.6) {
					fuzzyResults.push({ entry, sim: ratio })
				} else if (nameQuery.length >= 5) {
					// Bigram similarity for longer strings
					const sim = bigramDice(nameQuery, entryNameLow)
					if (sim >= 0.65) {
						fuzzyResults.push({ entry, sim })
					}
				}
			}

			fuzzyResults.sort((a, b) => b.sim - a.sim)
			for (const { entry, sim } of fuzzyResults.slice(0, needed)) {
				const typeBonus = parsed.type && entry.type === parsed.type ? 5 : 0
				addResult(entry, Math.round(30 + sim * 30) + typeBonus, 'fuzzy')
			}
		}

		// Sort by score descending, then by name length ascending
		results.sort((a, b) => {
			if (b.score !== a.score) return b.score - a.score
			return a.entry.fullName.length - b.entry.fullName.length
		})

		return results.slice(0, limit)
	}

	/**
	 * Check if a street exists in the dictionary (exact or near-exact match).
	 * Returns the best match or null.
	 */
	async findExact(streetName: string): Promise<StreetEntry | null> {
		await this.ensureLoaded()
		if (!streetName) return null

		const lower = streetName.trim().toLowerCase()

		// Direct fullName match
		const direct = this.entries.get(lower)
		if (direct) return direct

		// Try parsing and matching by name+type
		const parsed = this._parseStreetString(streetName)
		if (parsed) {
			const key = parsed.fullName.toLowerCase()
			const found = this.entries.get(key)
			if (found) return found
		}

		return null
	}

	/**
	 * Find the closest match for a street name (for typo detection).
	 * Returns null if no close match found.
	 */
	async findClosest(
		streetName: string,
		threshold: number = 0.7
	): Promise<{
		entry: StreetEntry
		similarity: number
		editDist: number
	} | null> {
		await this.ensureLoaded()
		if (!streetName || streetName.length < 3) return null

		const parsed = parseQuery(streetName)
		const nameQuery = parsed.name

		let bestMatch: StreetEntry | null = null
		let bestSim = 0
		let bestDist = Infinity

		for (const entry of this.entries.values()) {
			const entryNameLow = entry.name.toLowerCase()

			// Skip very different lengths
			if (Math.abs(entryNameLow.length - nameQuery.length) > 5) continue

			// Type filter if type was specified
			if (parsed.type && entry.type !== parsed.type) continue

			const dist = editDistance(nameQuery, entryNameLow)
			const maxLen = Math.max(nameQuery.length, entryNameLow.length)
			const ratio = 1 - dist / maxLen

			if (ratio > bestSim && ratio >= threshold) {
				bestSim = ratio
				bestDist = dist
				bestMatch = entry
			}

			// Also check bigram for longer names
			if (nameQuery.length >= 5) {
				const sim = bigramDice(nameQuery, entryNameLow)
				if (sim > bestSim && sim >= threshold) {
					bestSim = sim
					bestDist = dist
					bestMatch = entry
				}
			}
		}

		if (!bestMatch) return null

		return {
			entry: bestMatch,
			similarity: bestSim,
			editDist: bestDist
		}
	}

	/* ── Learning ── */

	/**
	 * Learn a new street from DaData response or manual input.
	 * Adds to in-memory dictionary and persists to DB.
	 */
	async learn(street: {
		name: string
		type: string
		region?: number
		settlement?: string
		source?: 'dadata' | 'manual'
	}): Promise<void> {
		const fullName = street.settlement
			? `${street.name} ${street.type} (${street.settlement})`
			: `${street.name} ${street.type}`

		const entry: StreetEntry = {
			name: street.name,
			type: street.type,
			fullName,
			region: street.region || 77,
			settlement: street.settlement,
			source: street.source || 'dadata'
		}

		// Add to in-memory
		this._addEntry(entry)
		this._buildPrefixIndex()

		// Persist to DB (fire and forget)
		try {
			await this.ensureCollection()
			await this.dbQuery('UPSERT_STREET',
				{
					name: street.name,
					type: street.type,
					fullName,
					region: street.region || 77,
					settlement: street.settlement || null,
					source: street.source || 'dadata',
					now: new Date().toISOString()
				}
			)
		} catch (e) {
			console.warn('[StreetDictionary] Failed to persist learned street:', e)
		}
	}

	/**
	 * Learn streets from a DaData suggestions response.
	 * Parses suggestion strings and adds to dictionary.
	 */
	async learnFromDaData(suggestions: string[]): Promise<void> {
		for (const sug of suggestions) {
			const parsed = this._parseStreetString(sug)
			if (parsed && parsed.name.length >= 2) {
				await this.learn({
					name: parsed.name,
					type: parsed.type,
					region: parsed.region,
					settlement: parsed.settlement,
					source: 'dadata'
				})
			}
		}
	}

	/* ── Stats ── */

	getStats(): {
		totalEntries: number
		bySeed: number
		byDb: number
		byDaData: number
		byManual: number
	} {
		let bySeed = 0;
			let byDb = 0;
			let byDaData = 0;
			let byManual = 0
		for (const entry of this.entries.values()) {
			switch (entry.source) {
				case 'seed':
					bySeed++
					break
				case 'db':
					byDb++
					break
				case 'dadata':
					byDaData++
					break
				case 'manual':
					byManual++
					break
			}
		}
		return { totalEntries: this.entries.size, bySeed, byDb, byDaData, byManual }
	}
}

/* ── Singleton ── */

let singleton: StreetDictionary | null = null

export function getStreetDictionary(
	dbQuery: (q: string, bv: Record<string, any>) => Promise<any[]>,
	ensureCollection: () => Promise<void>
): StreetDictionary {
	if (!singleton) {
		singleton = new StreetDictionary(dbQuery, ensureCollection)
	}
	return singleton
}
