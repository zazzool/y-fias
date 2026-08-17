// Package garzip читает выгрузку ГАР ФИАС, не скачивая её целиком.
//
// Полный архив gar_xml.zip — 53 ГБ, и в нём лежат все 89 регионов. Нам нужны
// два: 77 (Москва) и 50 (Московская область). Сервер ФНС отдаёт
// Accept-Ranges: bytes, а ZIP устроен так, что оглавление (central directory)
// лежит в конце файла. Значит можно прочитать оглавление по диапазону, найти
// смещения нужных записей и выкачать только их.
//
// archive/zip из стандартной библиотеки принимает io.ReaderAt и сам разбирает
// оглавление, поэтому достаточно реализовать io.ReaderAt поверх HTTP Range —
// дальше всё работает само, включая ZIP64.
package garzip

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// HTTPReaderAt — io.ReaderAt поверх HTTP-запросов с заголовком Range.
type HTTPReaderAt struct {
	ctx    context.Context
	client *http.Client
	url    string
	size   int64

	// Учёт фактически скачанного: главный ответ на вопрос
	// «сколько мы сэкономили против 53 ГБ».
	bytesFetched atomic.Int64
	requests     atomic.Int64
}

// NewHTTPReaderAt запрашивает размер файла и проверяет поддержку Range.
func NewHTTPReaderAt(ctx context.Context, client *http.Client, url string) (*HTTPReaderAt, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HEAD %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HEAD %s: код %d", url, resp.StatusCode)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		return nil, errors.New("сервер не поддерживает Range: придётся качать архив целиком")
	}
	size, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || size <= 0 {
		return nil, fmt.Errorf("не удалось определить размер файла: %q", resp.Header.Get("Content-Length"))
	}

	return &HTTPReaderAt{ctx: ctx, client: client, url: url, size: size}, nil
}

// Size — полный размер удалённого файла.
func (r *HTTPReaderAt) Size() int64 { return r.size }

// Stats — сколько байт и запросов реально ушло в сеть.
func (r *HTTPReaderAt) Stats() (bytes, requests int64) {
	return r.bytesFetched.Load(), r.requests.Load()
}

// ReadAt реализует io.ReaderAt.
//
// Повтор при сетевых сбоях обязателен: чтение оглавления и записей — это сотни
// запросов по нестабильному каналу, и один обрыв не должен ронять час работы.
func (r *HTTPReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || off >= r.size {
		return 0, io.EOF
	}

	end := off + int64(len(p)) - 1
	if end >= r.size {
		end = r.size - 1
	}
	want := int(end - off + 1)

	const attempts = 4
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-r.ctx.Done():
				return 0, r.ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}

		n, err := r.fetch(p[:want], off, end)
		if err == nil {
			if n < len(p) {
				return n, io.EOF
			}
			return n, nil
		}
		lastErr = err
	}
	return 0, fmt.Errorf("чтение диапазона %d-%d после %d попыток: %w", off, end, attempts, lastErr)
}

func (r *HTTPReaderAt) fetch(p []byte, off, end int64) (int, error) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// 206 — сервер учёл Range. 200 означает, что он его проигнорировал и шлёт
	// весь файл: 53 ГБ в буфер на несколько килобайт. Обрываем немедленно.
	if resp.StatusCode != http.StatusPartialContent {
		return 0, fmt.Errorf("ожидался код 206, получен %d", resp.StatusCode)
	}

	n, err := io.ReadFull(resp.Body, p)
	r.requests.Add(1)
	r.bytesFetched.Add(int64(n))
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return n, err
	}
	return n, nil
}
