package parse

// Коды качества разбора.
//
// Модель заимствована у DaData (docs/01-competitors.md §2.1) и расширена
// доменными шкалами. Суть заимствования: сервис никогда не отвечает «вот
// адрес», он отвечает «вот адрес и вот насколько я в нём уверен», причём
// по нескольким НЕЗАВИСИМЫМ шкалам.
//
// Это не украшение, а архитектурный ответ на аварию §8.2: система молча
// подменила номер дома, потому что у неё не было способа сказать «я не
// уверена». Ортогональные шкалы такой способ дают.

// ParseQC — уверенность в разборе строки.
type ParseQC uint8

const (
	ParseOK        ParseQC = 0 // разобрано уверенно
	ParseWithTail  ParseQC = 1 // остался нераспознанный хвост
	ParseGarbage   ParseQC = 2 // мусор: разбирать нечего
	ParseAmbiguous ParseQC = 3 // есть альтернативные прочтения
)

// MatchQC — как найдено совпадение в справочнике.
//
// Нулевое значение означает «НЕ ПРОВЕРЯЛОСЬ», а не «точное совпадение».
// Порядок здесь не косметический: сначала было MatchExact = 0, и эндпоинт
// разбора, который справочник вообще не трогает, сообщал «street: exact» —
// то есть заявлял уверенность, которой у него не было. Ровно от этого класса
// ошибок и заводились коды качества (§8.2).
type MatchQC uint8

const (
	MatchUnchecked MatchQC = iota
	MatchExact
	MatchPrefix
	MatchFuzzy
	MatchNone
)

// HouseQC — существование дома.
//
// Значение HouseNotInGar заимствовано у DaData (qc_house = 10) и важно именно
// для нас: справочник ФНС не содержит части реальных строений (§8.7 описи), а
// вызов туда всё равно приедет. Наивный валидатор на этом ломается.
// Нулевое значение — «НЕ ПРОВЕРЯЛОСЬ». Раньше нулём был HouseInGar, и любой
// ответ, где дом не сверялся со справочником, утверждал, что дом там есть.
type HouseQC uint8

const (
	HouseUnchecked HouseQC = iota // не сверялся
	HouseInGar                    // дом есть в ГАР
	HouseNotInGar                 // дом реален, но в ГАР его нет
	HouseUnknown                  // улица известна, дома её не загружены
)

// Confidence — уверенность в выводимом значении (район, ПНД).
//
// При Low сервис ОБЯЗАН вернуть кандидатов и не подставлять ничего молча:
// 181 улица пересекает границы районов, и на них тихая подстановка врёт.
type Confidence uint8

const (
	ConfNone Confidence = iota
	ConfLow
	ConfHigh
)

// Source — откуда взялось значение. Обязателен в каждом ответе: интерфейс
// должен уметь показать происхождение, а разбор аварии — восстановить путь.
type Source uint8

const (
	SourceNone      Source = iota
	SourceUser             // ввёл человек
	SourceLocal            // наша база адресов
	SourceGar              // справочник ГАР
	SourceOSM              // геометрия OpenStreetMap
	SourceNeighbors        // вывод по соседним домам
	SourceDaData           // внешний сервис
	SourceLearned          // выучено из исправлений человека
)

func (s Source) String() string {
	switch s {
	case SourceUser:
		return "user"
	case SourceLocal:
		return "local"
	case SourceGar:
		return "gar"
	case SourceOSM:
		return "osm"
	case SourceNeighbors:
		return "neighbors"
	case SourceDaData:
		return "dadata"
	case SourceLearned:
		return "learned"
	}
	return "none"
}

// Quality — сводка по разобранному адресу.
type Quality struct {
	Parse  ParseQC
	Street MatchQC
	House  HouseQC
	Source Source
}
