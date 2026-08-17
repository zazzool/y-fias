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
var magic = [8]byte{'Y', 'F', 'S', 'N', 'A', 'P', '0', '2'}

// Header — начало файла.
type Header struct {
	Version int64
	BuiltAt time.Time
	Streets int32
	CRC     uint32
}

// Write сохраняет улицы в файл.
//
// Запись идёт во временный файл и завершается переименованием: иначе
// прерванный ингест оставил бы половину снапшота под тем же именем, и сервис
// поднялся бы на нём.
func Write(path string, streets []index.Street, version int64) (Header, error) {
	tmp := path + ".tmp"

	f, err := os.Create(tmp)
	if err != nil {
		return Header{}, err
	}
	defer os.Remove(tmp) // на случай ошибки; после Rename файла уже нет

	h, err := writeTo(f, streets, version)
	if err != nil {
		f.Close()
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

func writeTo(w io.Writer, streets []index.Street, version int64) (Header, error) {
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
		}
	}

	// Полезная нагрузка собирается целиком в память, чтобы контрольная сумма
	// покрывала И пул строк, И записи. Считать её только по пулу было
	// недостаточно: порча в записях проходила незамеченной, а именно там
	// лежат смещения — по битому смещению читается чужая строка, и сервис
	// отвечает правдоподобно и неверно.
	payload := make([]byte, 0, len(pool)+len(recs)*recSize)
	payload = append(payload, pool...)

	var scratch [recSize]byte
	for _, r := range recs {
		putPair(scratch[0:], r.name)
		putPair(scratch[8:], r.typ)
		putPair(scratch[16:], r.region)
		putPair(scratch[24:], r.district)
		putPair(scratch[32:], r.settlement)
		binary.LittleEndian.PutUint32(scratch[40:], r.calls)
		payload = append(payload, scratch[:]...)
	}

	bw := bufio.NewWriterSize(w, 1<<20)
	hdr := Header{
		Version: version,
		BuiltAt: time.Now(),
		Streets: int32(len(streets)),
		CRC:     crc32.ChecksumIEEE(payload),
	}

	if _, err := bw.Write(magic[:]); err != nil {
		return hdr, err
	}
	for _, v := range []any{
		hdr.Version,
		hdr.BuiltAt.UnixNano(),
		hdr.Streets,
		hdr.CRC,
		int64(len(pool)),
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

// recSize — размер одной записи: пять пар (смещение, длина) плюс счётчик.
const recSize = 5*8 + 4

func putPair(b []byte, p [2]uint32) {
	binary.LittleEndian.PutUint32(b, p[0])
	binary.LittleEndian.PutUint32(b[4:], p[1])
}

// Read загружает улицы из файла.
func Read(path string) ([]index.Street, Header, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, Header{}, err
	}
	return decode(data)
}

func decode(data []byte) ([]index.Street, Header, error) {
	var hdr Header

	const headerSize = 8 + 8 + 8 + 4 + 4 + 8
	if len(data) < headerSize {
		return nil, hdr, fmt.Errorf("файл короче заголовка: %d байт", len(data))
	}
	if got := [8]byte(data[:8]); got != magic {
		if string(got[:6]) == "YFSNAP" {
			return nil, hdr, fmt.Errorf(
				"снапшот версии формата %s, поддерживается %s — пересоберите: make snapshot",
				got[6:], magic[6:])
		}
		return nil, hdr, fmt.Errorf("не снапшот Y-FIAS: неверная сигнатура")
	}

	p := 8
	hdr.Version = int64(binary.LittleEndian.Uint64(data[p:]))
	p += 8
	hdr.BuiltAt = time.Unix(0, int64(binary.LittleEndian.Uint64(data[p:])))
	p += 8
	hdr.Streets = int32(binary.LittleEndian.Uint32(data[p:]))
	p += 4
	hdr.CRC = binary.LittleEndian.Uint32(data[p:])
	p += 4
	poolLen := int(binary.LittleEndian.Uint64(data[p:]))
	p += 8

	if poolLen < 0 || p+poolLen > len(data) {
		return nil, hdr, fmt.Errorf("повреждён пул строк: длина %d", poolLen)
	}

	need := int(hdr.Streets) * recSize
	if p+poolLen+need > len(data) {
		return nil, hdr, fmt.Errorf("файл обрывается: нужно %d байт, есть %d",
			poolLen+need, len(data)-p)
	}

	// Контрольная сумма отделяет повреждённый файл от несовместимого и
	// покрывает всю полезную нагрузку, включая смещения в записях.
	payload := data[p : p+poolLen+need]
	if got := crc32.ChecksumIEEE(payload); got != hdr.CRC {
		return nil, hdr, fmt.Errorf("контрольная сумма не совпала: %08x против %08x",
			got, hdr.CRC)
	}

	pool := data[p : p+poolLen]
	p += poolLen

	str := func(off, ln uint32) (string, error) {
		if ln == 0 {
			return "", nil
		}
		if int(off)+int(ln) > len(pool) {
			return "", fmt.Errorf("смещение %d+%d вне пула длиной %d", off, ln, len(pool))
		}
		return string(pool[off : off+ln]), nil
	}

	streets := make([]index.Street, hdr.Streets)
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
				return nil, hdr, fmt.Errorf("улица %d: %w", i, err)
			}
		}

		streets[i] = index.Street{
			Name:       fields[0],
			Type:       fields[1],
			Region:     fields[2],
			District:   fields[3],
			Settlement: fields[4],
			Calls:      binary.LittleEndian.Uint32(data[base+5*8:]),
		}
	}
	return streets, hdr, nil
}
