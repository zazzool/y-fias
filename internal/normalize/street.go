// Package normalize — ЕДИНСТВЕННЫЙ источник правил нормализации адреса.
//
// Здесь объявлены типы улиц и алгоритмы приведения строк к сравнимому виду.
// SQL-функции street_key() и search_norm() не пишутся руками, а ГЕНЕРИРУЮТСЯ
// из этого пакета (go:generate ./gen). CI падает, если sql/normalize.sql
// разошёлся с Go.
//
// Причина такой строгости — §8.5 инвентаризации: список типов улиц жил в трёх
// местах (SQL-функция, orgMatcher.ts, addressService.ts) и разошёлся. Проверка
// на реальных данных (testdata/reference/README.md) показала, что копии
// реализуют три разных алгоритма, а одна из них не работает вовсе.
//
// Эталон поведения — копия A, SQL-функция street_key() из миграции 0029.
package normalize

//go:generate go run ./gen -out ../../sql/normalize.sql

import (
	"sort"
	"strings"
	"unicode"
)

// StreetType — тип улицы: каноническое название и все написания, которые
// встречаются во вводе и в базе.
type StreetType struct {
	Canon   string
	Aliases []string
}

// streetTypes — единственное объявление. Правится только здесь.
//
// Состав взят из street_key() (миграция 0029, 27 токенов) и дополнен теми, что
// в ней отсутствовали, но встречаются в адресах Москвы и области. Токены,
// которые отбрасываются при построении ключа, — это ВСЕ Aliases плюс Canon.
var streetTypes = []StreetType{
	{Canon: "улица", Aliases: []string{"ул"}},
	{Canon: "шоссе", Aliases: []string{"ш", "шос"}},
	{Canon: "проспект", Aliases: []string{"пр", "прт", "пркт", "прсп", "просп"}},
	{Canon: "переулок", Aliases: []string{"пер", "прк"}},
	{Canon: "площадь", Aliases: []string{"пл"}},
	{Canon: "бульвар", Aliases: []string{"б", "бр", "бул", "бульв"}},
	{Canon: "набережная", Aliases: []string{"наб"}},
	{Canon: "проезд", Aliases: []string{"прд", "проез"}},
	{Canon: "тупик", Aliases: []string{"туп"}},
	{Canon: "аллея", Aliases: []string{"ал"}},
	{Canon: "линия", Aliases: nil},
	{Canon: "квартал", Aliases: []string{"кв", "кварт"}},
	{Canon: "микрорайон", Aliases: []string{"мкр", "мкрн", "мк"}},

	// Ниже — типы, которых в миграции 0029 не было. Они встречаются в адресах
	// Московской области, где 42 наших адреса и где точность нужна не меньше:
	// одноимённые СНТ разных районов различаются только контекстом.
	{Canon: "территория", Aliases: []string{"тер"}},
	{Canon: "просека", Aliases: nil},
	{Canon: "спуск", Aliases: nil},
	{Canon: "тракт", Aliases: nil},
	{Canon: "кольцо", Aliases: nil},
	{Canon: "магистраль", Aliases: nil},
	{Canon: "заезд", Aliases: nil},
	{Canon: "въезд", Aliases: nil},
}

// streetTypeTokens — множество всех токенов-типов, включая канонические.
var streetTypeTokens = func() map[string]bool {
	m := make(map[string]bool, len(streetTypes)*3)
	for _, t := range streetTypes {
		m[t.Canon] = true
		for _, a := range t.Aliases {
			m[a] = true
		}
	}
	return m
}()

// StreetTypeTokens возвращает отсортированный список токенов-типов.
// Используется генератором SQL и тестами; порядок детерминирован, чтобы
// сгенерированный SQL не менялся от запуска к запуску.
func StreetTypeTokens() []string {
	out := make([]string, 0, len(streetTypeTokens))
	for t := range streetTypeTokens {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// IsStreetType сообщает, является ли токен типом улицы.
func IsStreetType(token string) bool { return streetTypeTokens[token] }

// SearchNorm приводит текст к виду, пригодному для триграммного поиска.
//
// Правила дословно повторяют search_norm() из миграции 0029:
//   - нижний регистр;
//   - ё → е (в базе встречаются оба написания, а триграммы у них не
//     пересекаются на месте буквы);
//   - дефис и тире → пробел (двойные названия должны находиться по любой части);
//   - всё, кроме букв и цифр, → пробел;
//   - схлопывание пробелов.
//
// «й» НАМЕРЕННО не сводится к «и». Решение записано в комментарии миграции
// 0029: это разные буквы, и опечатки такого рода закрывает нечёткий слой
// поиска, а не нормализация. Копия из orgMatcher.ts это правило нарушала.
func SearchNorm(s string) string {
	if s == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(s))

	prevSpace := true // подавляет ведущие пробелы
	for _, r := range s {
		r = unicode.ToLower(r)

		switch {
		case r == 'ё':
			r = 'е'
		case r == '-' || r == '–' || r == '—':
			r = ' '
		}

		if !isAlnum(r) {
			r = ' '
		}

		if r == ' ' {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteByte(' ')
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}

	return strings.TrimRight(b.String(), " ")
}

// isAlnum повторяет класс [a-zа-я0-9] из SQL: латиница, кириллица, цифры.
// unicode.IsLetter здесь не годится — он пропустил бы, например, греческие
// буквы, а SQL-функция их отбрасывает, и ключи разошлись бы.
func isAlnum(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'а' && r <= 'я':
		return true
	case r >= '0' && r <= '9':
		return true
	}
	return false
}

// StreetKey — ключ улицы: без типа и без учёта порядка слов.
//
// «улица Саляма Адиля» = «Саляма Адиля ул.» = «саляма адиля».
//
// Слова сортируются — именно это делает ключ независимым от порядка. В базе
// встречаются оба написания («3-я Парковая улица» и «Парковая 3-я улица»), и
// без сортировки они порождают дубликаты: 64 группы из 81 в проде
// (docs/04-stage0-findings.md §5).
// Пост-нормализация (postNormalize) применяется ТОЛЬКО здесь, но не в
// SearchNorm: последняя питает триграммные индексы, и менять её семантику
// значит перестраивать индексы на проде. StreetKey — ключ тождества, его
// поведение мы улучшаем сознательно.
func StreetKey(s string) string {
	norm := postNormalize(SearchNorm(s))
	if norm == "" {
		return ""
	}

	words := strings.Split(norm, " ")
	kept := words[:0]
	for _, w := range words {
		if w == "" || streetTypeTokens[w] {
			continue
		}
		kept = append(kept, w)
	}

	// Улица, целиком состоящая из типа («Линия», «Аллея» как название), не
	// должна давать пустой ключ — иначе она сольётся со всеми такими же.
	if len(kept) == 0 {
		return norm
	}

	sort.Strings(kept)
	return strings.Join(kept, " ")
}

// SplitStreetType отделяет тип улицы от названия, где бы тип ни стоял.
//
// Возвращает название и канонический тип («улица», «шоссе»); если тип не
// найден — второе значение пустое. Нужен для разбора пользовательского ввода:
// в ГАР название и тип хранятся раздельно (NAME и TYPENAME), а диспетчер
// вводит их одной строкой.
func SplitStreetType(s string) (name, typ string) {
	norm := postNormalize(SearchNorm(s))
	if norm == "" {
		return "", ""
	}

	words := strings.Split(norm, " ")

	// Типом считается ПОСЛЕДНЕЕ подходящее слово, а не первое.
	//
	// В «Большая Набережная улица» типами выглядят оба слова — «набережная»
	// и «улица», — но тип здесь только второе, а «Набережная» это название.
	// Разбор по первому совпадению давал «большая улица» + тип «набережная»,
	// то есть название улицы включало слово «улица».
	typeAt := -1
	for i, w := range words {
		if _, ok := canonOf(w); ok {
			typeAt = i
		}
	}
	if typeAt < 0 {
		return norm, ""
	}

	// Единственное слово и то оказалось типом: это название («Линия»).
	if len(words) == 1 {
		return norm, ""
	}

	typ, _ = canonOf(words[typeAt])
	kept := make([]string, 0, len(words)-1)
	kept = append(kept, words[:typeAt]...)
	kept = append(kept, words[typeAt+1:]...)
	return strings.Join(kept, " "), typ
}

// canonOf по любому написанию типа возвращает канонический.
var canonByAlias = func() map[string]string {
	m := make(map[string]string, len(streetTypes)*3)
	for _, t := range streetTypes {
		m[t.Canon] = t.Canon
		for _, a := range t.Aliases {
			m[a] = t.Canon
		}
	}
	return m
}()

func canonOf(token string) (string, bool) {
	c, ok := canonByAlias[token]
	return c, ok
}
