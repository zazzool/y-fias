// Package snapshot читает и пишет файл поискового снапшота.
//
// Формат намеренно простой: заголовок, пул строк, записи фиксированной
// структуры со смещениями в пул. Индексы (точный, префиксный, биграммный) в
// файл НЕ пишутся — они восстанавливаются при загрузке. Так файл втрое
// меньше, а сборка индексов занимает доли секунды и происходит в отдельном
// процессе ингеста либо при старте, но не на горячем пути.
//
// Деплой — копирование файла, откат — замена файла предыдущим.
package snapshot

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"time"

	"github.com/zazzool/y-fias/internal/index"
)

// magic — сигнатура и версия ФОРМАТА (последние два байта).
//
// Версию обязательно поднимать при любом изменении раскладки файла. Иначе
// старый снапшот читается новым кодом и падает на контрольной сумме, а
// сообщение «сумма не совпала» уводит в сторону: выглядит как повреждение
// диска, хотя файл цел и просто написан по прежним правилам.
//
//	01 — первый формат
//	02 — контрольная сумма покрывает не только пул строк, но и записи
//	03 — добавлена секция домов
//	04 — добавлены идентификаторы ГАР (нужны для дельта-синхронизации)
var magic = [8]byte{'Y', 'F', 'S', 'N', 'A', 'P', '0', '4'}

// Header — начало файла.
type Header struct {
	Version int64
	BuiltAt time.Time
	Streets int32
	Houses  int32
	CRC     uint32
}

// Data — содержимое снапшота.
type Data struct {
	Streets []index.Street
	// Houses — дома по позиции улицы.
	//
	// Нужны, чтобы отличать «дома нет в справочнике ФНС» от «дом не найден»
	// (HouseQC), и для вывода района по соседним домам. Хранятся отдельной
	// секцией: их 2.3 млн против 85 тыс. улиц, и грузить их нужно не всегда.
	//
	// Тип берётся из index, а не заводится свой: снапшот и так зависит от
	// index, а два почти одинаковых типа пришлось бы всё время
	// перекладывать друг в друга.
	Houses map[uint32][]index.HouseRef
}

// Write сохраняет улицы в файл.
//
// Запись идёт во временный файл и завершается переименованием: иначе
// прерванный ингест оставил бы половину снапшота под тем же именем, и сервис
// поднялся бы на нём.
func Write(path string, d Data, version int64) (Header, error) {
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return Header{}, err
	}
	defer func() { _ = os.Remove(tmp) }() // на случай ошибки; после Rename файла уже нет

	h, err := writeTo(f, d, version)
	if err != nil {
		_ = f.Close()
		return Header{}, err
	}
	if err := f.Close(); err != nil {
		return Header{}, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return Header{}, fmt.Errorf("переименование %s: %w", tmp, err)
	}
	return h, nil
}

func writeTo(w io.Writer, d Data, version int64) (Header, error) {
	streets := d.Streets
	// Пул строк: каждое уникальное значение хранится один раз. Названия
	// районов и населённых пунктов повторяются тысячами, поэтому выигрыш
	// заметный.
	pool := make([]byte, 0, len(streets)*48)
	offsets := map[string][2]uint32{}

	put := func(s string) [2]uint32 {
		if s == "" {
			return [2]uint32{0, 0}
		}
		if o, ok := offsets[s]; ok {
			return o
		}
		o := [2]uint32{uint32(len(pool)), uint32(len(s))}
		pool = append(pool, s...)
		offsets[s] = o
		return o
	}

	type rec struct {
		name, typ, region, district, settlement [2]uint32
		calls                                   uint32
		objectID, parentID                      int64
	}
	recs := make([]rec, len(streets))
	for i, st := range streets {
		recs[i] = rec{
			name:       put(st.Name),
			typ:        put(st.Type),
			region:     put(st.Region),
			district:   put(st.District),
			settlement: put(st.Settlement),
			calls:      st.Calls,
			objectID:   st.ObjectID,
			parentID:   st.ParentID,
		}
	}

	// Дома: для каждой улицы — свой пул номеров и диапазон в нём.
	//
	// Номера домов почти не повторяются между улицами, поэтому общий пул
	// строк им не нужен; хватает одного сплошного буфера с разделителями.
	var (
		housePool  []byte
		houseIDs   []int64
		houseRange = make([][2]uint32, len(streets))
		houseCount int
	)
	for i := range streets {
		list := d.Houses[uint32(i)]
		if len(list) == 0 {
			continue
		}
		start := uint32(len(housePool))
		for _, h := range list {
			if h.Canonical == "" {
				continue
			}
			housePool = append(housePool, h.Canonical...)
			housePool = append(housePool, 0) // разделитель
			houseIDs = append(houseIDs, h.ObjectID)
			houseCount++
		}
		// Улица, у которой все номера оказались пустыми, диапазона не
		// получает. Иначе запись и чтение расходятся: писатель считает её
		// улицей с домами, а читатель пропускает пустой диапазон. Ровно
		// это поймала проверка после ингеста — 14 улиц из 71 032.
		if n := uint32(len(housePool)) - start; n > 0 {
			houseRange[i] = [2]uint32{start, n}
		}
	}

	// Полезная нагрузка собирается целиком в память, чтобы контрольная сумма
	// покрывала И пул строк, И записи. Считать её только по пулу было
	// недостаточно: порча в записях проходила незамеченной, а именно там
	// лежат смещения — по битому смещению читается чужая строка, и сервис
	// отвечает правдоподобно и неверно.
	payload := make([]byte, 0, len(pool)+len(recs)*recSize+len(housePool)+len(streets)*8)
	payload = append(payload, pool...)

	var scratch [recSize]byte
	for _, r := range recs {
		putPair(scratch[0:], r.name)
		putPair(scratch[8:], r.typ)
		putPair(scratch[16:], r.region)
		putPair(scratch[24:], r.district)
		putPair(scratch[32:], r.settlement)
		binary.LittleEndian.PutUint32(scratch[40:], r.calls)
		binary.LittleEndian.PutUint64(scratch[44:], uint64(r.objectID))
		binary.LittleEndian.PutUint64(scratch[52:], uint64(r.parentID))
		payload = append(payload, scratch[:]...)
	}

	var pair [8]byte
	for _, r := range houseRange {
		putPair(pair[:], r)
		payload = append(payload, pair[:]...)
	}
	payload = append(payload, housePool...)

	// Идентификаторы домов идут отдельным массивом в том же порядке, что и
	// строки в пуле: так пул остаётся простым, а разбор — последовательным.
	var idBuf [8]byte
	for _, id := range houseIDs {
		binary.LittleEndian.PutUint64(idBuf[:], uint64(id))
		payload = append(payload, idBuf[:]...)
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	hdr := Header{
		Version: version,
		BuiltAt: time.Now(),
		Streets: int32(len(streets)),
		Houses:  int32(houseCount),
		CRC:     crc32.ChecksumIEEE(payload),
	}

	if _, err := bw.Write(magic[:]); err != nil {
		return hdr, err
	}
	for _, v := range []any{
		hdr.Version,
		hdr.BuiltAt.UnixNano(),
		hdr.Streets,
		hdr.Houses,
		hdr.CRC,
		int64(len(pool)),
		int64(len(housePool)),
	} {
		if err := binary.Write(bw, binary.LittleEndian, v); err != nil {
			return hdr, err
		}
	}
	if _, err := bw.Write(payload); err != nil {
		return hdr, err
	}
	return hdr, bw.Flush()
}

// recSize — размер одной записи: пять пар (смещение, длина), счётчик вызовов
// и два идентификатора ГАР.
const recSize = 5*8 + 4 + 8 + 8

func putPair(b []byte, p [2]uint32) {
	binary.LittleEndian.PutUint32(b, p[0])
	binary.LittleEndian.PutUint32(b[4:], p[1])
}

// Read загружает снапшот из файла.
func Read(path string) (Data, Header, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Data{}, Header{}, err
	}
	return decode(data)
}

func decode(data []byte) (Data, Header, error) {
	var (
		hdr Header
		out Data
	)

	const headerSize = 8 + 8 + 8 + 4 + 4 + 4 + 8 + 8
	if len(data) < headerSize {
		return out, hdr, fmt.Errorf("файл короче заголовка: %d байт", len(data))
	}
	if got := [8]byte(data[:8]); got != magic {
		if string(got[:6]) == "YFSNAP" {
			return out, hdr, fmt.Errorf(
				"снапшот версии формата %s, поддерживается %s — пересоберите: make snapshot",
				got[6:], magic[6:])
		}
		return out, hdr, fmt.Errorf("не снапшот Y-FIAS: неверная сигнатура")
	}

	p := 8
	hdr.Version = int64(binary.LittleEndian.Uint64(data[p:]))
	p += 8
	hdr.BuiltAt = time.Unix(0, int64(binary.LittleEndian.Uint64(data[p:])))
	p += 8
	hdr.Streets = int32(binary.LittleEndian.Uint32(data[p:]))
	p += 4
	hdr.Houses = int32(binary.LittleEndian.Uint32(data[p:]))
	p += 4
	hdr.CRC = binary.LittleEndian.Uint32(data[p:])
	p += 4
	poolLen := int(binary.LittleEndian.Uint64(data[p:]))
	p += 8
	housePoolLen := int(binary.LittleEndian.Uint64(data[p:]))
	p += 8

	if poolLen < 0 || housePoolLen < 0 {
		return out, hdr, fmt.Errorf("повреждены длины секций: %d, %d", poolLen, housePoolLen)
	}

	recs := int(hdr.Streets) * recSize
	ranges := int(hdr.Streets) * 8
	houseIDs := int(hdr.Houses) * 8
	total := poolLen + recs + ranges + housePoolLen + houseIDs
	if p+total > len(data) {
		return out, hdr, fmt.Errorf("файл обрывается: нужно %d байт, есть %d",
			total, len(data)-p)
	}

	// Контрольная сумма отделяет повреждённый файл от несовместимого и
	// покрывает всю полезную нагрузку, включая смещения в записях.
	if got := crc32.ChecksumIEEE(data[p : p+total]); got != hdr.CRC {
		return out, hdr, fmt.Errorf("контрольная сумма не совпала: %08x против %08x",
			got, hdr.CRC)
	}

	pool := data[p : p+poolLen]
	p += poolLen
	need := recs

	str := func(off, ln uint32) (string, error) {
		if ln == 0 {
			return "", nil
		}
		if int(off)+int(ln) > len(pool) {
			return "", fmt.Errorf("смещение %d+%d вне пула длиной %d", off, ln, len(pool))
		}
		return string(pool[off : off+ln]), nil
	}

	out.Streets = make([]index.Street, hdr.Streets)
	streets := out.Streets
	for i := range streets {
		base := p + i*recSize
		g := func(k int) (uint32, uint32) {
			o := base + k*8
			return binary.LittleEndian.Uint32(data[o:]), binary.LittleEndian.Uint32(data[o+4:])
		}
		no, nl := g(0)
		to, tl := g(1)
		ro, rl := g(2)
		do, dl := g(3)
		so, sl := g(4)

		var err error
		fields := [5]string{}
		for k, pair := range [5][2]uint32{{no, nl}, {to, tl}, {ro, rl}, {do, dl}, {so, sl}} {
			if fields[k], err = str(pair[0], pair[1]); err != nil {
				return out, hdr, fmt.Errorf("улица %d: %w", i, err)
			}
		}

		streets[i] = index.Street{
			Name:       fields[0],
			Type:       fields[1],
			Region:     fields[2],
			District:   fields[3],
			Settlement: fields[4],
			Calls:      binary.LittleEndian.Uint32(data[base+5*8:]),
			ObjectID:   int64(binary.LittleEndian.Uint64(data[base+5*8+4:])),
			ParentID:   int64(binary.LittleEndian.Uint64(data[base+5*8+12:])),
		}
	}
	p += need

	// Секция домов.
	if hdr.Houses > 0 {
		rangesAt := p
		housesAt := p + ranges
		idsAt := housesAt + housePoolLen
		housePool := data[housesAt : housesAt+housePoolLen]

		out.Houses = make(map[uint32][]index.HouseRef, hdr.Streets/4)

		// Идентификаторы идут в том же порядке, что и строки в пуле,
		// поэтому читаются одним последовательным счётчиком.
		idx := 0
		for i := range streets {
			off := binary.LittleEndian.Uint32(data[rangesAt+i*8:])
			ln := binary.LittleEndian.Uint32(data[rangesAt+i*8+4:])
			if ln == 0 {
				continue
			}
			if int(off)+int(ln) > len(housePool) {
				return out, hdr, fmt.Errorf("улица %d: дома вне пула", i)
			}

			nums := splitZ(housePool[off : off+ln])
			list := make([]index.HouseRef, 0, len(nums))
			for _, n := range nums {
				var id int64
				if idx < int(hdr.Houses) {
					id = int64(binary.LittleEndian.Uint64(data[idsAt+idx*8:]))
				}
				idx++
				list = append(list, index.HouseRef{ObjectID: id, Canonical: n})
			}
			out.Houses[uint32(i)] = list
		}
	}

	return out, hdr, nil
}

// splitZ разбивает буфер, где строки разделены нулевым байтом.
func splitZ(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(b) {
		out = append(out, string(b[start:]))
	}
	return out
}
