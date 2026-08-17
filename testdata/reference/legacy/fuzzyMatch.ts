/**
 * Fuzzy matching utilities for Russian text.
 * Used by both StreetDictionary and NameDictionary.
 */

import { distance as levenshtein } from 'fastest-levenshtein'

/* ── Bigram Dice coefficient ── */

export function bigramDice(a: string, b: string): number {
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

/* ── Edit distance (Damerau-Levenshtein) ── */

export function editDistance(a: string, b: string): number {
	if (a === b) return 0
	if (!a.length) return b.length
	if (!b.length) return a.length
	if (Math.abs(a.length - b.length) > 4) return Math.max(a.length, b.length)

	const m: number[][] = []
	for (let i = 0; i <= a.length; i++) m[i] = [i]
	for (let j = 0; j <= b.length; j++) m[0][j] = j

	for (let i = 1; i <= a.length; i++) {
		for (let j = 1; j <= b.length; j++) {
			const cost = a[i - 1] === b[j - 1] ? 0 : 1
			m[i][j] = Math.min(m[i - 1][j] + 1, m[i][j - 1] + 1, m[i - 1][j - 1] + cost)
			if (i > 1 && j > 1 && a[i - 1] === b[j - 2] && a[i - 2] === b[j - 1])
				m[i][j] = Math.min(m[i][j], m[i - 2][j - 2] + cost)
		}
	}
	return m[a.length][b.length]
}

/**
 * Fast Levenshtein from `fastest-levenshtein` (C++ optimized).
 * Use this for simple distance checks; use editDistance() when
 * transposition handling (Damerau) is important.
 */
export { levenshtein }

/* ── Weighted similarity score ── */

/**
 * Combined similarity score using edit distance and bigram Dice.
 * Returns 0..1 (1 = identical).
 */
export function similarity(a: string, b: string): number {
	if (a === b) return 1
	const al = a.toLowerCase()
	const bl = b.toLowerCase()
	if (al === bl) return 1

	const dist = editDistance(al, bl)
	const maxLen = Math.max(al.length, bl.length)
	const editSim = 1 - dist / maxLen
	const diceSim = bigramDice(al, bl)

	// Weighted average: edit distance is more reliable for short strings,
	// bigram for longer strings
	return al.length <= 5 ? editSim * 0.7 + diceSim * 0.3 : editSim * 0.4 + diceSim * 0.6
}

/* ── Russian-specific normalization ── */

/** Normalize ё→е for comparison */
export function normalizeYo(s: string): string {
	return s.replace(/ё/g, 'е').replace(/Ё/g, 'Е')
}

/** Check if string is purely Cyrillic (allows spaces, hyphens) */
export function isCyrillic(s: string): boolean {
	return /^[\u0400-\u04FF\s\-.']+$/.test(s)
}

/** Check if string contains Latin characters */
export function hasLatin(s: string): boolean {
	return /[a-zA-Z]/.test(s)
}
