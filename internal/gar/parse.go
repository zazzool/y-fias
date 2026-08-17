package gar

import (
	"encoding/xml"
	"fmt"
	"io"
)

// Виды справочников, которые нам нужны.
//
// Всё остальное из архива не читается, и это не экономия ради экономии:
// AS_APARTMENTS и AS_ROOMS описывают квартиры и комнаты, а квартира по
// инварианту 6 — свойство ВЫЗОВА, а не адреса. Загружать их значило бы
// строить модель, которую мы сознательно отвергли.
const (
	KindAddrObj      = "AS_ADDR_OBJ"
	KindHouses       = "AS_HOUSES"
	KindAdmHierarchy = "AS_ADM_HIERARCHY"
	KindMunHierarchy = "AS_MUN_HIERARCHY"
	KindHouseTypes   = "AS_HOUSE_TYPES"
	KindAddHouseType = "AS_ADDHOUSE_TYPES"
	KindObjectLevels = "AS_OBJECT_LEVELS"
)

// Уровни объектов ГАР (AS_OBJECT_LEVELS).
const (
	LevelRegion      = 1
	LevelAdmDistrict = 2
	LevelMunDistrict = 3
	LevelSettlement2 = 4
	LevelCity        = 5
	LevelLocality    = 6
	LevelPlanning    = 7
	LevelStreet      = 8
	LevelStead       = 9
	LevelBuilding    = 10
	LevelInnerCity   = 17
)

// Object — адресный объект: город, посёлок, элемент планировки, улица.
type Object struct {
	ObjectID int64
	Name     string // «Бакунинский»
	TypeName string // «пер» — ГАР хранит тип ОТДЕЛЬНО от названия
	Level    uint8
	Region   string
}

// House — дом. Структура повторяет схему ГАР: основной номер плюс до двух
// дополнительных частей с типом.
type House struct {
	ObjectID int64
	Num      string
	Type     uint8 // 1 владение · 2 дом · 3 домовладение · 4 гараж
	AddNum1  string
	AddType1 uint8 // 1 корпус · 2 строение · 3 сооружение · 4 литера
	AddNum2  string
	AddType2 uint8
	Region   string
}

// Link — связь «объект -> родитель» из административной иерархии.
type Link struct {
	ObjectID int64
	ParentID int64
}

// Filter управляет тем, что берётся из потока.
type Filter struct {
	// Levels — какие уровни объектов оставлять. Пусто означает «все».
	Levels map[uint8]bool
	// Objects — какие ObjectID оставлять (для иерархии и домов).
	// Пусто означает «все».
	Objects map[int64]bool
}

func (f Filter) allowLevel(l uint8) bool {
	if len(f.Levels) == 0 {
		return true
	}
	return f.Levels[l]
}

func (f Filter) allowObject(id int64) bool {
	if len(f.Objects) == 0 {
		return true
	}
	return f.Objects[id]
}

// Stats — что произошло при разборе записи.
type Stats struct {
	Seen    int // элементов в файле
	Kept    int // взято
	Stale   int // отброшено как устаревшая версия
	Skipped int // отброшено фильтром
}

// ParseObjects читает AS_ADDR_OBJ.
func ParseObjects(r io.Reader, region string, f Filter, fn func(Object)) (Stats, error) {
	return parse(r, func(a attrs) bool {
		lvl := uint8(a.int64("LEVEL"))
		if !f.allowLevel(lvl) {
			return false
		}
		id := a.int64("OBJECTID")
		if !f.allowObject(id) {
			return false
		}
		fn(Object{
			ObjectID: id,
			Name:     a.str("NAME"),
			TypeName: a.str("TYPENAME"),
			Level:    lvl,
			Region:   region,
		})
		return true
	})
}

// ParseHouses читает AS_HOUSES.
func ParseHouses(r io.Reader, region string, f Filter, fn func(House)) (Stats, error) {
	return parse(r, func(a attrs) bool {
		id := a.int64("OBJECTID")
		if !f.allowObject(id) {
			return false
		}
		fn(House{
			ObjectID: id,
			Num:      a.str("HOUSENUM"),
			Type:     uint8(a.int64("HOUSETYPE")),
			AddNum1:  a.str("ADDNUM1"),
			AddType1: uint8(a.int64("ADDTYPE1")),
			AddNum2:  a.str("ADDNUM2"),
			AddType2: uint8(a.int64("ADDTYPE2")),
			Region:   region,
		})
		return true
	})
}

// ParseLinks читает AS_ADM_HIERARCHY.
func ParseLinks(r io.Reader, f Filter, fn func(Link)) (Stats, error) {
	return parse(r, func(a attrs) bool {
		id := a.int64("OBJECTID")
		if !f.allowObject(id) {
			return false
		}
		fn(Link{ObjectID: id, ParentID: a.int64("PARENTOBJID")})
		return true
	})
}

// parse — общий потоковый обход.
//
// Фильтр актуальности вынесен сюда и применяется ко всем видам справочников:
// ГАР хранит ВСЕ версии объекта — правки, переименования, упразднения. По
// Москве и области действующих записей 14.2 млн, устаревших — столько же.
// Без ISACTIVE=1 и ISACTUAL=1 объём вырос бы примерно втрое, а в подсказках
// появились бы улицы, переименованные двадцать лет назад.
func parse(r io.Reader, take func(attrs) bool) (Stats, error) {
	var st Stats

	dec := xml.NewDecoder(r)
	dec.Strict = false

	var root bool
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return st, fmt.Errorf("разбор XML: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if !root {
			root = true // корневой элемент: <ADDRESSOBJECTS>, <HOUSES>, ...
			continue
		}

		st.Seen++
		a := attrsOf(se)

		if !a.isCurrent() {
			st.Stale++
			continue
		}
		if take(a) {
			st.Kept++
		} else {
			st.Skipped++
		}
	}
	return st, nil
}

/* ── Доступ к атрибутам ────────────────────────────────────────── */

type attrs []xml.Attr

func attrsOf(se xml.StartElement) attrs { return attrs(se.Attr) }

func (a attrs) str(name string) string {
	for _, at := range a {
		if at.Name.Local == name {
			return at.Value
		}
	}
	return ""
}

func (a attrs) int64(name string) int64 {
	s := a.str(name)
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return n
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

// isCurrent — действующая ли это редакция объекта.
func (a attrs) isCurrent() bool {
	if a.str("ISACTIVE") != "1" {
		return false
	}
	// У иерархии ISACTUAL может отсутствовать — тогда достаточно ISACTIVE.
	if v := a.str("ISACTUAL"); v != "" && v != "1" {
		return false
	}
	return true
}
