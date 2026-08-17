/**
 * Эмпирическая проверка расхождений трёх копий нормализации улицы (§8.5).
 *
 * A — street_key() из миграции 0029 (SQL, проверяется отдельно на Postgres)
 * B — normalizeStreetKey() из server/domain/orgMatcher.ts
 * C — normalizeStreetForCompare() из server/domain/addressService.ts
 *
 * Копии скопированы дословно на 17.08.2026, коммит 887071cd.
 * Запуск: node testdata/reference/divergence-probe.mjs
 */

/* ── Копия B: orgMatcher.ts ── */
const B_TOKENS = new Set([
	'ул', 'улица', 'ш', 'шоссе', 'пр', 'пркт', 'проспект', 'пер', 'переулок',
	'пл', 'площадь', 'бр', 'бульвар', 'наб', 'набережная', 'прд', 'проезд',
	'туп', 'тупик', 'аллея', 'линия'
])

export function B(str) {
	const tokens = String(str || '')
		.toLowerCase()
		.replace(/ё/g, 'е')
		.replace(/й/g, 'и')
		.replace(/\(.*?\)/g, ' ')
		.replace(/[.,\-/\\]+/g, ' ')
		.split(/\s+/)
		.filter(t => t && !B_TOKENS.has(t))
	return tokens.sort().join(' ')
}

/* ── Копия C: addressService.ts ── */
const C_RE = [
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

export function C(str) {
	if (!str) return ''
	let s = str.toLowerCase()
	for (const re of C_RE) s = s.replace(re, '')
	return s
		.replace(/\(.*?\)/g, '')
		.replace(/[.,\-\s]+/g, ' ')
		.trim()
}

/* ── Наблюдения ── */

const cases = [
	['порядок слов', 'улица Тверская', 'Тверская улица'],
	['порядок слов', 'улица Саляма Адиля', 'Саляма Адиля ул.'],
	['буква ё', 'Хорошёвское шоссе', 'Хорошевское шоссе'],
	['буква й', 'Гвардейская улица', 'Гвардеиская улица'],
	['скобки: разные СНТ', 'Вишнёвая улица (СНТ Лесная поляна, Раменский р-н)',
		'Вишнёвая улица (СНТ Ромашка, Ногинский р-н)'],
]

const single = [
	'улица Тверская', 'Тверская улица', 'Зеленоградский квартал 1',
	'1-я линия', 'Ленинский проспект', 'Мичуринский проспект, Олимпийская деревня',
]

console.log('=== Разбор отдельных значений ===')
console.log('ВХОД'.padEnd(46) + '| B: orgMatcher'.padEnd(26) + '| C: addressService')
console.log('-'.repeat(100))
for (const s of single) {
	console.log(s.padEnd(46) + '| ' + B(s).padEnd(24) + '| ' + C(s))
}

console.log('\n=== Считают ли копии два написания одним адресом? ===')
console.log('СЛУЧАЙ'.padEnd(22) + '| B'.padEnd(8) + '| C')
console.log('-'.repeat(100))
for (const [label, x, y] of cases) {
	const b = B(x) === B(y) ? 'ДА' : 'НЕТ'
	const c = C(x) === C(y) ? 'ДА' : 'НЕТ'
	const flag = b === c ? '' : '   <-- РАСХОЖДЕНИЕ'
	console.log(label.padEnd(22) + '| ' + b.padEnd(6) + '| ' + c + flag)
	console.log('  ' + x)
	console.log('    B=' + JSON.stringify(B(x)) + '  C=' + JSON.stringify(C(x)))
	console.log('  ' + y)
	console.log('    B=' + JSON.stringify(B(y)) + '  C=' + JSON.stringify(C(y)))
}
