/**
 * QWERTY ↔ ЙЦУКЕН keyboard layout mapping.
 * Detects and corrects text typed in wrong layout.
 */

const LATIN_TO_CYRILLIC: Record<string, string> = {
	q: 'й',
	w: 'ц',
	e: 'у',
	r: 'к',
	t: 'е',
	y: 'н',
	u: 'г',
	i: 'ш',
	o: 'щ',
	p: 'з',
	'[': 'х',
	']': 'ъ',
	a: 'ф',
	s: 'ы',
	d: 'в',
	f: 'а',
	g: 'п',
	h: 'р',
	j: 'о',
	k: 'л',
	l: 'д',
	';': 'ж',
	"'": 'э',
	z: 'я',
	x: 'ч',
	c: 'с',
	v: 'м',
	b: 'и',
	n: 'т',
	m: 'ь',
	',': 'б',
	'.': 'ю',
	'`': 'ё',
	Q: 'Й',
	W: 'Ц',
	E: 'У',
	R: 'К',
	T: 'Е',
	Y: 'Н',
	U: 'Г',
	I: 'Ш',
	O: 'Щ',
	P: 'З',
	'{': 'Х',
	'}': 'Ъ',
	A: 'Ф',
	S: 'Ы',
	D: 'В',
	F: 'А',
	G: 'П',
	H: 'Р',
	J: 'О',
	K: 'Л',
	L: 'Д',
	':': 'Ж',
	'"': 'Э',
	Z: 'Я',
	X: 'Ч',
	C: 'С',
	V: 'М',
	B: 'И',
	N: 'Т',
	M: 'Ь',
	'<': 'Б',
	'>': 'Ю',
	'~': 'Ё'
}

/**
 * Convert QWERTY-typed text to ЙЦУКЕН Cyrillic.
 * E.g., "Bdfyjd" → "Иванов"
 */
export function latinToCyrillic(text: string): string {
	let result = ''
	for (const ch of text) {
		result += LATIN_TO_CYRILLIC[ch] || ch
	}
	return result
}

/**
 * Check if the string looks like it was typed in the wrong keyboard layout.
 * Returns true if the string contains mostly Latin characters that map to Cyrillic.
 */
export function isLikelyWrongLayout(text: string): boolean {
	if (!text || text.length < 2) return false
	let mappable = 0
	let latin = 0
	for (const ch of text) {
		if (/[a-zA-Z]/.test(ch)) {
			latin++
			if (LATIN_TO_CYRILLIC[ch]) mappable++
		}
	}
	// If >70% of chars are Latin and all Latin chars are mappable
	return latin > 0 && latin / text.replace(/[\s\-.]/g, '').length > 0.7 && mappable === latin
}
