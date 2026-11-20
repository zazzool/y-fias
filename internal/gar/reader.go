// Package gar читает выгрузку Государственного адресного реестра.
//
// Пакет намеренно отделён от команд: тем же кодом пользуются полный ингест
// (cmd/yfias-ingest) и синхронизация по дельтам (§11 архитектуры). Дельта
// устроена так же, как полный архив — те же 1 739 записей, та же схема XML, —
// поэтому переиспользование здесь не абстракция ради абстракции, а факт.
package gar

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/zazzool/y-fias/internal/garzip"
)

// APIURL — служба ФНС, отдающая ссылки на актуальную выгрузку.
//
// Домен fias-file.nalog.ru, а не fias.nalog.ru: второй недоступен извне,
// проверено 17.08.2026.
const APIURL = "https://fias-file.nalog.ru/WebServices/Public/GetLastDownloadFileInfo"

// AllVersionsURL — список всех доступных версий. Нужен синхронизации, чтобы
// применить дельты по порядку.
const AllVersionsURL = "https://fias-file.nalog.ru/WebServices/Public/GetAllDownloadFileInfo"

// VersionInfo — одна публикация ГАР.
type VersionInfo struct {
	VersionID   int    `json:"VersionId"`
	TextVersion string `json:"TextVersion"`
	Date        string `json:"Date"`
	FullURL     string `json:"GarXMLFullURL"`
	DeltaURL    string `json:"GarXMLDeltaURL"`
}

// LatestVersion запрашивает последнюю публикацию.
func LatestVersion(ctx context.Context, client *http.Client) (VersionInfo, error) {
	var v VersionInfo
	err := getJSON(ctx, client, APIURL, &v)
	return v, err
}

// AllVersions возвращает все доступные публикации, от новой к старой.
//
// ФНС держит онлайн около полугода (56 версий на 17.08.2026). Если наша
// версия старше самой старой доступной, дельты не помогут и нужна полная
// пересборка — это штатная ветка, а не ошибка (§11.3 архитектуры).
func AllVersions(ctx context.Context, client *http.Client) ([]VersionInfo, error) {
	var vs []VersionInfo
	err := getJSON(ctx, client, AllVersionsURL, &vs)
	return vs, err
}

func getJSON(ctx context.Context, client *http.Client, url string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("запрос %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: код %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		return fmt.Errorf("разбор ответа %s: %w", url, err)
	}
	return nil
}

/* ── Доступ к архиву ───────────────────────────────────────────── */

// Archive — открытая выгрузка ГАР.
type Archive struct {
	zr      *zip.Reader
	stats   func() (bytes, requests int64)
	size    int64
	cleanup func() error
}

// OpenURL открывает архив по HTTP, не скачивая его целиком.
//
// Полный архив — 53 ГБ, из которых нужны около 700 МБ. Сервер ФНС отдаёт
// Accept-Ranges, а ZIP хранит оглавление в конце файла, поэтому archive/zip
// поверх io.ReaderAt читает только те записи, которые запрошены.
// Блочный кэш обязателен: без него распаковщик flate породил бы миллионы
// HTTP-запросов вместо десятков.
func OpenURL(ctx context.Context, client *http.Client, url string) (*Archive, error) {
	ra, err := garzip.NewHTTPReaderAt(ctx, client, url)
	if err != nil {
		return nil, err
	}
	br := garzip.NewBufferedReaderAt(ra, ra.Size(), 8<<20, 6)

	zr, err := zip.NewReader(br, ra.Size())
	if err != nil {
		return nil, fmt.Errorf("разбор ZIP: %w", err)
	}
	return &Archive{
		zr:      zr,
		stats:   ra.Stats,
		size:    ra.Size(),
		cleanup: func() error { return nil },
	}, nil
}

// OpenFile открывает ранее скачанный архив с диска.
func OpenFile(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	zr, err := zip.NewReader(f, st.Size())
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("разбор ZIP: %w", err)
	}
	return &Archive{
		zr:      zr,
		size:    st.Size(),
		stats:   func() (int64, int64) { return 0, 0 },
		cleanup: f.Close,
	}, nil
}

func (a *Archive) Close() error { return a.cleanup() }

// Size — полный размер архива.
func (a *Archive) Size() int64 { return a.size }

// Fetched — сколько байт и запросов реально ушло в сеть.
func (a *Archive) Fetched() (bytes, requests int64) { return a.stats() }

// Entry — запись архива, отнесённая к виду справочника и региону.
type Entry struct {
	File   *zip.File
	Kind   string // AS_ADDR_OBJ, AS_HOUSES, AS_ADM_HIERARCHY, ...
	Region string // «77», «50»; пусто для общих справочников
}

// Entries отбирает записи нужного вида и регионов.
// Пустой список регионов означает «все».
func (a *Archive) Entries(kind string, regions []string) []Entry {
	want := map[string]bool{}
	for _, r := range regions {
		want[strings.TrimLeft(r, "0")] = true
	}

	var out []Entry
	for _, f := range a.zr.File {
		e := classify(f.Name)
		if e.Kind != kind {
			continue
		}
		if e.Region != "" && len(want) > 0 && !want[e.Region] {
			continue
		}
		e.File = f
		out = append(out, e)
	}
	return out
}

// classify разбирает имя записи вида «77/AS_HOUSES_20260814_uuid.XML».
func classify(name string) Entry {
	var e Entry

	base := name
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		if dir := name[:i]; isDigits(dir) {
			e.Region = strings.TrimLeft(dir, "0")
			if e.Region == "" {
				e.Region = dir
			}
		}
		base = name[i+1:]
	}

	up := strings.TrimSuffix(strings.ToUpper(base), ".XML")
	var parts []string
	for _, p := range strings.Split(up, "_") {
		// Имя вида KIND_ДАТА_UUID: с даты начинается «хвост».
		if len(p) >= 8 && isDigits(p) {
			break
		}
		parts = append(parts, p)
	}
	e.Kind = strings.Join(parts, "_")
	if e.Kind == "" {
		e.Kind = up
	}
	return e
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Open открывает запись для потокового чтения.
//
// Держать распакованный файл в памяти нельзя: AS_HOUSES по Московской
// области — 1.2 ГБ.
func (e Entry) Open() (io.ReadCloser, error) { return e.File.Open() }

// Compressed — размер записи в архиве.
func (e Entry) Compressed() int64 { return int64(e.File.CompressedSize64) }

// ParseVersionDate извлекает дату публикации из строки вида «14.08.2026».
func ParseVersionDate(s string) (time.Time, error) {
	return time.Parse("02.01.2006", s)
}
