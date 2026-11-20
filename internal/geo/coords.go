package geo

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
)

// Координаты адресов.
//
// Опись §2.3: координат нет ни у одного из 10 694 адресов, поэтому район
// выводится эвристикой по соседним домам, а она заведомо врёт на 181 улице,
// пересекающей границы районов. Координаты закрывают это точно.
//
// Хранятся ОТДЕЛЬНО от справочника, и это не оплошность. Источник у них
// другой (OpenStreetMap, а не ГАР), частота обновления другая, и покрытие
// заведомо неполное. Смешивать их со справочником значило бы связать два
// независимых цикла обновления в один.

// Coord — точка адреса.
type Coord struct {
	Lon float64
	Lat float64
}

// coordKey — ключ адреса: район, улица и канонический номер дома.
//
// Улица задаётся КЛЮЧОМ, а не названием: у одной улицы десяток написаний, и
// сопоставление по тексту потеряло бы большую часть точек.
//
// Район в ключе обязателен, и это выяснилось на данных. Без него 116 448
// координат «покрыли» 425 032 дома справочника: «Центральная улица, 1» есть в
// сотнях населённых пунктов, и каждый такой дом получал бы чужую точку. Для
// адресного сервиса неотложки это худший вид ошибки — координата выглядит
// достоверной и уводит бригаду в другой район.
//
// Район точки вычисляется по её же координате попаданием в полигон, поэтому
// дополнительных данных не требуется.
type coordKey struct {
	District string
	Street   string
	House    string
}

// Coords — координаты адресов с поиском по улице и дому.
type Coords struct {
	m map[coordKey]Coord
}

func NewCoords() *Coords {
	return &Coords{m: map[coordKey]Coord{}}
}

func (c *Coords) Len() int {
	if c == nil {
		return 0
	}
	return len(c.m)
}

// Put добавляет координату.
func (c *Coords) Put(district, streetKey, houseCanonical string, p Coord) {
	if streetKey == "" || houseCanonical == "" {
		return
	}
	c.m[coordKey{normDistrict(district), streetKey, houseCanonical}] = p
}

// Lookup ищет координату адреса.
func (c *Coords) Lookup(district, streetKey, houseCanonical string) (Coord, bool) {
	if c == nil {
		return Coord{}, false
	}
	p, ok := c.m[coordKey{normDistrict(district), streetKey, houseCanonical}]
	return p, ok
}

// normDistrict сглаживает написание района: ГАР пишет «Хорошево-Мневники»,
// OSM — «Хорошёво-Мнёвники».
func normDistrict(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch r {
		case 'ё':
			r = 'е'
		case 'Ё':
			r = 'Е'
		case '-':
			r = ' '
		}
		if r >= 'А' && r <= 'Я' {
			r += 'а' - 'А'
		}
		out = append(out, r)
	}
	return string(out)
}

// LookupAnyDistrict ищет адрес БЕЗ учёта района и возвращает районы, в
// которых он нашёлся.
//
// Нужна для разбора расхождений: если адрес есть в другом районе, значит
// записанный у нас район, скорее всего, неверен. Опись §2.4 прямо
// предупреждает, что историческим районам доверять нельзя, и это способ
// узнать, насколько.
func (c *Coords) LookupAnyDistrict(streetKey, houseCanonical string) map[string]Coord {
	if c == nil {
		return nil
	}
	var out map[string]Coord
	for k, v := range c.m {
		if k.Street == streetKey && k.House == houseCanonical {
			if out == nil {
				out = map[string]Coord{}
			}
			out[k.District] = v
		}
	}
	return out
}

// LookupStreet возвращает любую координату на улице.
//
// Нужна как запасной вариант: если точного дома нет, район улицы почти всегда
// определяется верно — кроме тех самых 181 улицы, что пересекают границы.
// Поэтому вызывающий обязан понижать уверенность, а не выдавать результат как
// точный.
func (c *Coords) LookupStreet(district, streetKey string) (Coord, int) {
	if c == nil {
		return Coord{}, 0
	}
	d := normDistrict(district)
	var (
		sum Coord
		n   int
	)
	for k, v := range c.m {
		if k.Street == streetKey && k.District == d {
			sum.Lon += v.Lon
			sum.Lat += v.Lat
			n++
		}
	}
	if n == 0 {
		return Coord{}, 0
	}
	return Coord{Lon: sum.Lon / float64(n), Lat: sum.Lat / float64(n)}, n
}

/* ── Файл координат ────────────────────────────────────────────── */

// Версия 2: в ключ добавлен район. Версия 1 без него давала чужие
// координаты домам одноимённых улиц в других населённых пунктах.
var coordsMagic = [8]byte{'Y', 'F', 'C', 'O', 'O', 'R', 'D', '2'}

// WriteCoords сохраняет координаты в файл.
//
// Формат простой и плоский: заголовок, затем записи «длина ключа улицы, ключ,
// длина номера, номер, широта, долгота». JSON здесь был бы вчетверо больше и
// разбирался бы секундами.
func WriteCoords(path string, c *Coords) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()

	// Порядок записей фиксируется сортировкой: иначе файл меняется при
	// каждой сборке из-за случайного обхода карты, и разницу не посмотреть.
	keys := make([]coordKey, 0, len(c.m))
	for k := range c.m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].District != keys[j].District {
			return keys[i].District < keys[j].District
		}
		if keys[i].Street != keys[j].Street {
			return keys[i].Street < keys[j].Street
		}
		return keys[i].House < keys[j].House
	})

	var body []byte
	var num [8]byte
	for _, k := range keys {
		p := c.m[k]
		body = binary.LittleEndian.AppendUint16(body, uint16(len(k.District)))
		body = append(body, k.District...)
		body = binary.LittleEndian.AppendUint16(body, uint16(len(k.Street)))
		body = append(body, k.Street...)
		body = binary.LittleEndian.AppendUint16(body, uint16(len(k.House)))
		body = append(body, k.House...)

		binary.LittleEndian.PutUint64(num[:], uint64(int64(p.Lon*1e7)))
		body = append(body, num[:]...)
		binary.LittleEndian.PutUint64(num[:], uint64(int64(p.Lat*1e7)))
		body = append(body, num[:]...)
	}

	bw := bufio.NewWriterSize(f, 1<<20)
	if _, err := bw.Write(coordsMagic[:]); err != nil {
		_ = f.Close()
		return err
	}
	for _, v := range []any{int64(len(keys)), crc32.ChecksumIEEE(body)} {
		if err := binary.Write(bw, binary.LittleEndian, v); err != nil {
			_ = f.Close()
			return err
		}
	}
	if _, err := bw.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ReadCoords загружает координаты.
func ReadCoords(path string) (*Coords, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 8+8+4 {
		return nil, fmt.Errorf("файл короче заголовка")
	}
	if [8]byte(data[:8]) != coordsMagic {
		return nil, fmt.Errorf("не файл координат Y-FIAS")
	}

	p := 8
	count := int(binary.LittleEndian.Uint64(data[p:]))
	p += 8
	sum := binary.LittleEndian.Uint32(data[p:])
	p += 4

	if got := crc32.ChecksumIEEE(data[p:]); got != sum {
		return nil, fmt.Errorf("контрольная сумма не совпала: %08x против %08x", got, sum)
	}

	c := NewCoords()
	body := data[p:]
	off := 0

	readStr := func() (string, error) {
		if off+2 > len(body) {
			return "", io.ErrUnexpectedEOF
		}
		n := int(binary.LittleEndian.Uint16(body[off:]))
		off += 2
		if off+n > len(body) {
			return "", io.ErrUnexpectedEOF
		}
		s := string(body[off : off+n])
		off += n
		return s, nil
	}

	for range count {
		district, err := readStr()
		if err != nil {
			return nil, fmt.Errorf("чтение района: %w", err)
		}
		street, err := readStr()
		if err != nil {
			return nil, fmt.Errorf("чтение ключа улицы: %w", err)
		}
		house, err := readStr()
		if err != nil {
			return nil, fmt.Errorf("чтение номера дома: %w", err)
		}
		if off+16 > len(body) {
			return nil, io.ErrUnexpectedEOF
		}
		lon := float64(int64(binary.LittleEndian.Uint64(body[off:]))) / 1e7
		lat := float64(int64(binary.LittleEndian.Uint64(body[off+8:]))) / 1e7
		off += 16

		c.m[coordKey{district, street, house}] = Coord{Lon: lon, Lat: lat}
	}
	return c, nil
}
