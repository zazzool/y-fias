package garzip

import (
	"fmt"
	"io"
	"sync"
)

// BufferedReaderAt — блочный кэш поверх io.ReaderAt.
//
// Зачем. Распаковщик flate читает мелкими порциями (сотни байт), а каждый
// такой вызов у HTTPReaderAt — это отдельный HTTP-запрос. На файле AS_HOUSES
// в 1.2 ГБ это миллионы запросов и часы ожидания.
//
// Читаем выровненными блоками по blockSize и держим небольшое окно последних
// блоков. Доступ при распаковке строго последовательный, поэтому окна в
// несколько блоков достаточно, а память под кэш ограничена сверху.
type BufferedReaderAt struct {
	src       io.ReaderAt
	size      int64
	blockSize int64
	maxBlocks int

	mu     sync.Mutex
	blocks map[int64][]byte
	order  []int64 // порядок появления, для вытеснения самого старого

	hits, misses int64
}

// NewBufferedReaderAt оборачивает src. blockSize и maxBlocks задают верхнюю
// границу памяти: blockSize * maxBlocks байт.
func NewBufferedReaderAt(src io.ReaderAt, size, blockSize int64, maxBlocks int) *BufferedReaderAt {
	if blockSize <= 0 {
		blockSize = 8 << 20
	}
	if maxBlocks <= 0 {
		maxBlocks = 4
	}
	return &BufferedReaderAt{
		src:       src,
		size:      size,
		blockSize: blockSize,
		maxBlocks: maxBlocks,
		blocks:    make(map[int64][]byte, maxBlocks),
	}
}

// Stats — попаданий и промахов кэша.
func (b *BufferedReaderAt) Stats() (hits, misses int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits, b.misses
}

func (b *BufferedReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= b.size {
		return 0, io.EOF
	}

	var written int
	for written < len(p) {
		cur := off + int64(written)
		if cur >= b.size {
			return written, io.EOF
		}

		idx := cur / b.blockSize
		blk, err := b.block(idx)
		if err != nil {
			return written, err
		}

		inBlock := cur - idx*b.blockSize
		if inBlock >= int64(len(blk)) {
			return written, io.EOF
		}
		written += copy(p[written:], blk[inBlock:])
	}
	return written, nil
}

func (b *BufferedReaderAt) block(idx int64) ([]byte, error) {
	b.mu.Lock()
	if blk, ok := b.blocks[idx]; ok {
		b.hits++
		b.mu.Unlock()
		return blk, nil
	}
	b.misses++
	b.mu.Unlock()

	start := idx * b.blockSize
	length := b.blockSize
	if start+length > b.size {
		length = b.size - start
	}

	buf := make([]byte, length)
	n, err := b.src.ReadAt(buf, start)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("чтение блока %d (смещение %d): %w", idx, start, err)
	}
	buf = buf[:n]

	b.mu.Lock()
	defer b.mu.Unlock()
	// Пока мы качали, блок мог появиться — тогда отдаём уже имеющийся.
	if existing, ok := b.blocks[idx]; ok {
		return existing, nil
	}
	b.blocks[idx] = buf
	b.order = append(b.order, idx)
	for len(b.order) > b.maxBlocks {
		delete(b.blocks, b.order[0])
		b.order = b.order[1:]
	}
	return buf, nil
}
