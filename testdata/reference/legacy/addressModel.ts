/**
 * Address Correction Model
 *
 * Обучающаяся модель предсказания верного написания адреса.
 * Учится на данных слияния адресов (source → target = wrong → correct).
 *
 * Слои предсказания (от точного к приблизительному):
 * 1. Direct lookup — точное совпадение с ранее виденной ошибкой
 * 2. Case-insensitive lookup — совпадение без учёта регистра
 * 3. Pattern rules — извлечённые правила трансформации (ул. → улица и т.п.)
 * 4. Normalized match — поиск по нормализованной форме
 * 5. Fuzzy match — нечёткое совпадение (bigram Dice ≥ 0.75)
 *
 * Данные хранятся в таблице address_corrections, модель кэшируется в памяти.
 */

/* ── Types ── */

import { formatHouseCanonical, normalizeStreetForCompare } from './addressService'

export interface CorrectionRecord {
	_key?: string
	wrong_str: string
	wrong_h: string
	correct_str: string
	correct_h: string
	source: 'merge' | 'manual'
	count: number
	created: string
	updated: string
}

export interface Prediction {
	str: string
	h: string
	confidence: number // 0–100
	reason: string
	corrections: number // how many training examples support this
}

/* ── Normalization (imported from addressService to avoid duplication) ── */

function normalizeStreet(str: string): string {
	return normalizeStreetForCompare(str)
}

function normalizeHouse(h: string): string {
	if (!h) return ''
	return formatHouseCanonical(h).toLowerCase()
}

/** Bigram Dice coefficient */
function bigramDice(a: string, b: string): number {
	if (a === b) return 1
	if (a.length < 2 || b.length < 2) return 0
	const biA = new Map<string, number>()
	for (let i = 0; i < a.length - 1; i++) {
		const bg = a.substring(i, i + 2)
		biA.set(bg, (biA.get(bg) || 0) + 1)
	}
	let inter = 0
	for (let i = 0; i < b.length - 1; i++) {
		const bg = b.substring(i, i + 2)
		const c = biA.get(bg)
		if (c && c > 0) {
			inter++
			biA.set(bg, c - 1)
		}
	}
	return (2 * inter) / (a.length - 1 + b.length - 1)
}

/* ── Pattern Extraction ── */

interface TransformRule {
	from: string // pattern in wrong string
	to: string // replacement in correct string
	count: number // how many corrections generated this rule
	scope: 'street' | 'house'
}

/**
 * Extract substitution patterns from a correction pair.
 * Example: "Потешная ул." → "Потешная улица" → rule { from: "ул.", to: "улица" }
 */
function extractPatterns(wrong: string, correct: string): Array<{ from: string; to: string }> {
	if (!wrong || !correct || wrong === correct) return []

	const patterns: Array<{ from: string; to: string }> = []
	const wL = wrong.toLowerCase().trim()
	const cL = correct.toLowerCase().trim()

	// Try to find the common prefix and suffix to isolate the changed part
	let prefixLen = 0
	while (prefixLen < wL.length && prefixLen < cL.length && wL[prefixLen] === cL[prefixLen])
		prefixLen++
	let suffixLen = 0
	while (
		suffixLen < wL.length - prefixLen &&
		suffixLen < cL.length - prefixLen &&
		wL[wL.length - 1 - suffixLen] === cL[cL.length - 1 - suffixLen]
	)
		suffixLen++

	const wDiff = wL.substring(prefixLen, wL.length - suffixLen).trim()
	const cDiff = cL.substring(prefixLen, cL.length - suffixLen).trim()

	if (wDiff || cDiff) {
		// Only add non-trivial patterns (at least one side has content)
		if (wDiff.length <= 15 && cDiff.length <= 15) {
			patterns.push({ from: wDiff, to: cDiff })
		}
	}

	// Word-level patterns: find whole words that differ
	const wWords = wL.split(/\s+/)
	const cWords = cL.split(/\s+/)
	if (wWords.length === cWords.length) {
		for (let i = 0; i < wWords.length; i++) {
			if (wWords[i] !== cWords[i]) {
				patterns.push({ from: wWords[i], to: cWords[i] })
			}
		}
	}

	return patterns
}

/* ── Model Class ── */

export class AddressCorrectionModel {
	// In-memory index of corrections
	private corrections: CorrectionRecord[] = []

	// Lookup maps for fast prediction
	private exactMap = new Map<string, CorrectionRecord[]>() // wrong_str||wrong_h → corrections
	private caseMap = new Map<string, CorrectionRecord[]>() // lower(wrong_str)||lower(wrong_h)
	private normMap = new Map<string, CorrectionRecord[]>() // normalized_str||normalized_h
	private streetExactMap = new Map<string, CorrectionRecord[]>() // wrong_str → corrections (any house)
	private streetCaseMap = new Map<string, CorrectionRecord[]>() // lower(wrong_str)

	// Learned transform rules
	private streetRules: TransformRule[] = []
	private houseRules: TransformRule[] = []

	// State
	private loaded = false
	private loading: Promise<void> | null = null
	private lastLoad = 0
	private readonly CACHE_TTL = 3 * 60 * 1000 // 3 min

	// eslint-disable-next-line no-useless-constructor
	constructor(
		private readonly dbQuery: (query: string, bindVars: Record<string, any>) => Promise<any[]>,
		private readonly ensureCollection: () => Promise<void>
	) {}

	/* ── Data Loading ── */

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
			await this.ensureCollection()
			const rows = await this.dbQuery('SELECT_ALL_CORRECTIONS', {})
			this.corrections = rows
			this._buildIndexes()
			this.loaded = true
			this.lastLoad = Date.now()
			console.log(
				`[AddressModel] Loaded ${rows.length} corrections, ${this.streetRules.length} street rules, ${this.houseRules.length} house rules`
			)
		} catch (e) {
			console.error('[AddressModel] Failed to load:', e)
			this.loaded = false
		}
	}

	private _buildIndexes(): void {
		this.exactMap.clear()
		this.caseMap.clear()
		this.normMap.clear()
		this.streetExactMap.clear()
		this.streetCaseMap.clear()

		const ruleMap = new Map<string, TransformRule>()

		for (const c of this.corrections) {
			// Exact
			const ek = `${c.wrong_str}||${c.wrong_h}`
			if (!this.exactMap.has(ek)) this.exactMap.set(ek, [])
			this.exactMap.get(ek)!.push(c)

			// Case-insensitive
			const ck = `${(c.wrong_str || '').toLowerCase()}||${(c.wrong_h || '').toLowerCase()}`
			if (!this.caseMap.has(ck)) this.caseMap.set(ck, [])
			this.caseMap.get(ck)!.push(c)

			// Normalized
			const nk = `${normalizeStreet(c.wrong_str)}||${normalizeHouse(c.wrong_h)}`
			if (!this.normMap.has(nk)) this.normMap.set(nk, [])
			this.normMap.get(nk)!.push(c)

			// Street-only (any house)
			if (!this.streetExactMap.has(c.wrong_str)) this.streetExactMap.set(c.wrong_str, [])
			this.streetExactMap.get(c.wrong_str)!.push(c)

			const sLow = (c.wrong_str || '').toLowerCase()
			if (!this.streetCaseMap.has(sLow)) this.streetCaseMap.set(sLow, [])
			this.streetCaseMap.get(sLow)!.push(c)

			// Extract patterns
			const streetPatterns = extractPatterns(c.wrong_str, c.correct_str)
			for (const p of streetPatterns) {
				const rk = `s:${p.from}→${p.to}`
				const existing = ruleMap.get(rk)
				if (existing) {
					existing.count += c.count
				} else {
					ruleMap.set(rk, { from: p.from, to: p.to, count: c.count, scope: 'street' })
				}
			}

			const housePatterns = extractPatterns(c.wrong_h, c.correct_h)
			for (const p of housePatterns) {
				const rk = `h:${p.from}→${p.to}`
				const existing = ruleMap.get(rk)
				if (existing) {
					existing.count += c.count
				} else {
					ruleMap.set(rk, { from: p.from, to: p.to, count: c.count, scope: 'house' })
				}
			}
		}

		// Sort rules by count (most seen first), filter trivial
		const allRules = [...ruleMap.values()].filter(r => r.from !== r.to)
		this.streetRules = allRules.filter(r => r.scope === 'street').sort((a, b) => b.count - a.count)
		this.houseRules = allRules.filter(r => r.scope === 'house').sort((a, b) => b.count - a.count)
	}

	/* ── Learning ── */

	/**
	 * Record a correction from merge operation.
	 * Called when addresses are merged: wrong (source) → correct (target).
	 */
	async learn(
		wrongStr: string,
		wrongH: string,
		correctStr: string,
		correctH: string,
		source: 'merge' | 'manual' = 'merge'
	): Promise<void> {
		// Skip if no actual change
		if (wrongStr === correctStr && wrongH === correctH) return
		if (!wrongStr && !wrongH) return

		const now = new Date().toISOString()

		try {
			await this.ensureCollection()
			// UPSERT: increment count if same correction already exists
			await this.dbQuery('UPSERT_CORRECTION', {
				ws: wrongStr || '',
				wh: wrongH || '',
				cs: correctStr || '',
				ch: correctH || '',
				source,
				now
			})
		} catch (e) {
			console.error('[AddressModel] learn failed:', e)
		}

		// Invalidate cache so next prediction uses fresh data
		this.loaded = false
	}

	/**
	 * Bulk learn from a merge operation (single query for all pairs).
	 * All source addresses are "wrong", target is "correct".
	 */
	async learnFromMerge(
		target: { str: string; h: string },
		sources: Array<{ str: string; h: string }>
	): Promise<void> {
		// Filter out no-ops
		const pairs = sources.filter(s => (s.str !== target.str || s.h !== target.h) && (s.str || s.h))
		if (!pairs.length) return

		const now = new Date().toISOString()

		try {
			await this.ensureCollection()
			await this.dbQuery('UPSERT_CORRECTIONS_BATCH', {
				pairs: pairs.map(s => ({ ws: s.str || '', wh: s.h || '' })),
				cs: target.str || '',
				ch: target.h || '',
				now
			})
		} catch (e) {
			console.error('[AddressModel] learnFromMerge failed:', e)
		}

		this.loaded = false
	}

	/* ── Prediction ── */

	/**
	 * Predict the correct address for given input.
	 * Returns null if no confident prediction.
	 */
	async predict(str: string, h: string): Promise<Prediction | null> {
		await this.ensureLoaded()

		if (!str && !h) return null
		if (this.corrections.length === 0) return null

		// Layer 1: Exact match
		const ek = `${str}||${h}`
		const exact = this.exactMap.get(ek)
		if (exact?.length) {
			const best = this._bestCorrection(exact)
			if (best.correct_str !== str || best.correct_h !== h) {
				return {
					str: best.correct_str,
					h: best.correct_h,
					confidence: Math.min(99, 85 + best.count * 3),
					reason: `Точное совпадение (${best.count} исправлений)`,
					corrections: best.count
				}
			}
		}

		// Layer 2: Case-insensitive match
		const ck = `${(str || '').toLowerCase()}||${(h || '').toLowerCase()}`
		const caseMatch = this.caseMap.get(ck)
		if (caseMatch?.length) {
			const best = this._bestCorrection(caseMatch)
			if (
				best.correct_str.toLowerCase() !== str.toLowerCase() ||
				best.correct_h.toLowerCase() !== h.toLowerCase()
			) {
				return {
					str: best.correct_str,
					h: best.correct_h,
					confidence: Math.min(95, 80 + best.count * 2),
					reason: `Совпадение без учёта регистра (${best.count} исправлений)`,
					corrections: best.count
				}
			}
			// Same after lowercase — might still want the exact case correction
			if (best.correct_str !== str || best.correct_h !== h) {
				return {
					str: best.correct_str,
					h: best.correct_h,
					confidence: Math.min(93, 78 + best.count * 2),
					reason: `Исправление регистра (${best.count} исправлений)`,
					corrections: best.count
				}
			}
		}

		// Layer 3: Pattern rules — apply known transformations
		const patternResult = this._applyPatternRules(str, h)
		if (patternResult && (patternResult.str !== str || patternResult.h !== h)) {
			// Verify the pattern-generated result exists in known corrections
			const verifyKey = `${(patternResult.str || '').toLowerCase()}||${(patternResult.h || '').toLowerCase()}`
			const verified = this.caseMap.has(verifyKey)
			if (verified || patternResult.confidence >= 60) {
				return patternResult
			}
		}

		// Layer 4: Normalized match
		const nk = `${normalizeStreet(str)}||${normalizeHouse(h)}`
		const normMatch = this.normMap.get(nk)
		if (normMatch?.length) {
			const best = this._bestCorrection(normMatch)
			if (best.correct_str !== str || best.correct_h !== h) {
				return {
					str: best.correct_str,
					h: best.correct_h,
					confidence: Math.min(85, 65 + best.count * 2),
					reason: `Совпадение после нормализации`,
					corrections: best.count
				}
			}
		}

		// Layer 5: Street-only match (different house is ok)
		const streetLow = (str || '').toLowerCase()
		const streetMatch = this.streetCaseMap.get(streetLow)
		if (streetMatch?.length) {
			const best = this._bestCorrection(streetMatch)
			if (best.correct_str !== str) {
				return {
					str: best.correct_str,
					h, // keep user's house
					confidence: Math.min(80, 55 + best.count * 2),
					reason: `Исправление улицы (${best.count} исправлений)`,
					corrections: best.count
				}
			}
		}

		// Layer 6: Fuzzy match — bigram similarity on street name
		const fzResult = this._fuzzyMatch(str, h)
		if (fzResult) return fzResult

		return null
	}

	/** Find best correction from a group (highest count) */
	private _bestCorrection(corrs: CorrectionRecord[]): CorrectionRecord {
		return corrs.reduce((best, c) => (c.count > best.count ? c : best), corrs[0])
	}

	/** Apply learned pattern rules to transform input */
	private _applyPatternRules(str: string, h: string): Prediction | null {
		if (!this.streetRules.length && !this.houseRules.length) return null

		let correctedStr = str
		let correctedH = h
		let totalCount = 0
		let applied = 0

		// Apply street rules (top 20 most frequent)
		for (const rule of this.streetRules.slice(0, 20)) {
			if (rule.count < 2) continue // need at least 2 examples
			const lower = correctedStr.toLowerCase()
			const idx = lower.indexOf(rule.from)
			if (idx !== -1) {
				// Preserve original case structure if possible
				correctedStr =
					correctedStr.substring(0, idx) + rule.to + correctedStr.substring(idx + rule.from.length)
				totalCount += rule.count
				applied++
			}
		}

		// Apply house rules
		for (const rule of this.houseRules.slice(0, 10)) {
			if (rule.count < 2) continue
			const lower = correctedH.toLowerCase()
			const idx = lower.indexOf(rule.from)
			if (idx !== -1) {
				correctedH =
					correctedH.substring(0, idx) + rule.to + correctedH.substring(idx + rule.from.length)
				totalCount += rule.count
				applied++
			}
		}

		if (applied === 0) return null

		return {
			str: correctedStr,
			h: correctedH,
			confidence: Math.min(75, 40 + Math.min(totalCount, 10) * 3 + applied * 5),
			reason: `Правило трансформации (${applied} правил, ${totalCount} примеров)`,
			corrections: totalCount
		}
	}

	/** Fuzzy match using bigram Dice on all known wrong street names */
	private _fuzzyMatch(str: string, h: string): Prediction | null {
		if (!str || str.length < 4) return null

		const inputLow = str.toLowerCase()
		let bestSim = 0
		let bestCorr: CorrectionRecord | null = null

		// Check all unique wrong streets
		for (const [wrongStrLow, corrs] of this.streetCaseMap) {
			if (wrongStrLow === inputLow) continue // already handled by case match
			const sim = bigramDice(inputLow, wrongStrLow)
			if (sim > bestSim && sim >= 0.75) {
				bestSim = sim
				bestCorr = this._bestCorrection(corrs)
			}
		}

		if (!bestCorr) return null

		return {
			str: bestCorr.correct_str,
			h: h || bestCorr.correct_h,
			confidence: Math.min(70, Math.round(35 + bestSim * 40)),
			reason: `Похожая улица (${Math.round(bestSim * 100)}% сходство)`,
			corrections: bestCorr.count
		}
	}

	/* ── Stats ── */

	getStats(): {
		totalCorrections: number
		uniquePatterns: number
		streetRules: number
		houseRules: number
	} {
		return {
			totalCorrections: this.corrections.length,
			uniquePatterns: this.corrections.reduce((sum, c) => sum + c.count, 0),
			streetRules: this.streetRules.length,
			houseRules: this.houseRules.length
		}
	}
}

/* ── Singleton ── */

let singleton: AddressCorrectionModel | null = null

export function getAddressCorrectionModel(
	dbQuery: (q: string, bv: Record<string, any>) => Promise<any[]>,
	ensureCollection: () => Promise<void>
): AddressCorrectionModel {
	if (!singleton) {
		singleton = new AddressCorrectionModel(dbQuery, ensureCollection)
	}
	return singleton
}
