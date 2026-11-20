// Команда garprobe — этап 0 плана: измерить, а не предположить.
//
// Вся архитектура Y-FIAS стоит на утверждении «справочник Москвы и МО влезает
// в оперативную память одного процесса» (§1 docs/02-architecture.md). Пока
// утверждение не проверено на реальных данных, всё остальное — гипотеза.
//
// Режимы:
//
//	-mode=list   оглавление архива: что лежит внутри и сколько весит
//	-mode=probe  разбор нужных регионов с замером памяти
package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/zazzool/y-fias/internal/garzip"
)

const apiURL = "https://fias-file.nalog.ru/WebServices/Public/GetLastDownloadFileInfo"

var (
	peekKind = flag.String("kind", "", "режим peek: показать только этот справочник")
	peekSize = flag.Int("peek-bytes", 1200, "режим peek: сколько байт прочитать")
)

func main() {
	var (
		mode    = flag.String("mode", "list", "режим: list | peek | probe")
		url     = flag.String("url", "", "URL архива gar_xml.zip (по умолчанию — из API ФНС)")
		regions = flag.String("regions", "77,50", "коды регионов через запятую")
		timeout = flag.Duration("timeout", 2*time.Hour, "общий лимит времени")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, *mode, *url, *regions); err != nil {
		fmt.Fprintf(os.Stderr, "\nОШИБКА: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, mode, url, regions string) error {
	client := &http.Client{Timeout: 10 * time.Minute}

	if url == "" {
		var err error
		url, err = latestGarURL(ctx, client)
		if err != nil {
			return err
		}
	}

	want := parseRegions(regions)
	fmt.Printf("Архив:   %s\n", url)
	fmt.Printf("Регионы: %s\n\n", strings.Join(sortedKeys(want), ", "))

	ra, err := garzip.NewHTTPReaderAt(ctx, client, url)
	if err != nil {
		return err
	}
	fmt.Printf("Размер архива: %s\n", humanBytes(ra.Size()))
	fmt.Print("Читаю оглавление... ")

	// Без блочного кэша распаковщик flate породил бы миллионы HTTP-запросов.
	br := garzip.NewBufferedReaderAt(ra, ra.Size(), 8<<20, 4)
	zr, err := zip.NewReader(br, ra.Size())
	if err != nil {
		return fmt.Errorf("разбор ZIP: %w", err)
	}
	cdBytes, cdReqs := ra.Stats()
	fmt.Printf("готово: %d записей, скачано %s за %d запросов\n\n",
		len(zr.File), humanBytes(cdBytes), cdReqs)

	switch mode {
	case "list":
		return listEntries(zr, want, ra.Size())
	case "peek":
		return peekEntries(zr, want, ra)
	case "probe":
		return runProbe(zr, want, ra)
	default:
		return fmt.Errorf("неизвестный режим %q", mode)
	}
}

// coreKinds — справочники, без которых адресный сервис не построить.
//
// Всё остальное в архиве нам не нужно, и это не экономия ради экономии:
// AS_APARTMENTS и AS_ROOMS описывают квартиры и комнаты, а квартира по
// инварианту 6 (§0 архитектуры) — свойство ВЫЗОВА, а не адреса. Загружать их
// значило бы строить модель, которую мы сознательно отвергли.
var coreKinds = map[string]string{
	"AS_ADDR_OBJ":          "адресные объекты: города, улицы",
	"AS_ADDR_OBJ_TYPES":    "типы адресных объектов (ул., пр-кт, ...)",
	"AS_HOUSES":            "дома с номерами, корпусами, строениями",
	"AS_HOUSE_TYPES":       "типы домов (дом, владение, домовладение)",
	"AS_ADDHOUSE_TYPES":    "дополнительные типы (корпус, строение, литера)",
	"AS_ADM_HIERARCHY":     "административная иерархия: улица -> район -> город",
	"AS_OBJECT_LEVELS":     "уровни объектов",
	"AS_ADDR_OBJ_DIVISION": "деление адресных объектов",
}

// peekEntries печатает начало каждого нужного файла: схему видно из данных,
// а не из документации, которая для ГАР местами расходится с выгрузкой.
func peekEntries(zr *zip.Reader, want map[string]bool, ra *garzip.HTTPReaderAt) error {
	peekBytes := *peekSize

	var shown int
	for _, f := range zr.File {
		e := classify(f.Name)
		if e.region != "" && !want[e.region] {
			continue
		}
		if _, ok := coreKinds[e.kind]; !ok {
			continue
		}
		if *peekKind != "" && !strings.EqualFold(e.kind, *peekKind) {
			continue
		}
		// По одному образцу на вид справочника.
		if seenKind[e.kind] {
			continue
		}
		seenKind[e.kind] = true

		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("открытие %s: %w", f.Name, err)
		}
		buf := make([]byte, peekBytes)
		n, _ := io.ReadFull(rc, buf)
		_ = rc.Close()

		fmt.Printf("\n=== %s (%s)\n", e.kind, coreKinds[e.kind])
		fmt.Printf("    %s, %s в архиве\n\n", f.Name, humanBytes(int64(f.CompressedSize64)))
		fmt.Print(describe(string(buf[:n])))
		shown++
	}

	b, r := ra.Stats()
	fmt.Printf("\nПоказано справочников: %d. Скачано всего: %s за %d запросов.\n",
		shown, humanBytes(b), r)
	return nil
}

var seenKind = map[string]bool{}

// describe показывает СХЕМУ фрагмента: набор атрибутов с примерами значений.
// Сырой XML для этого не годится — строки ГАР длиной в сотни символов, и
// нужные атрибуты (ADDNUM1, ADDTYPE1) прячутся в хвосте.
func describe(s string) string {
	dec := xml.NewDecoder(strings.NewReader(s))
	dec.Strict = false

	order := []string{}
	samples := map[string]map[string]bool{}
	var root string

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if root == "" {
			root = se.Name.Local
			continue
		}
		for _, a := range se.Attr {
			if _, seen := samples[a.Name.Local]; !seen {
				order = append(order, a.Name.Local)
				samples[a.Name.Local] = map[string]bool{}
			}
			if len(samples[a.Name.Local]) < 3 && a.Value != "" {
				samples[a.Name.Local][a.Value] = true
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "    <%s> ... %d атрибутов:\n", root, len(order))
	for _, name := range order {
		vals := make([]string, 0, 3)
		for v := range samples[name] {
			if len(v) > 28 {
				v = v[:28] + "…"
			}
			vals = append(vals, v)
		}
		sort.Strings(vals)
		fmt.Fprintf(&b, "      %-14s %s\n", name, strings.Join(vals, " · "))
	}
	return b.String()
}

// entry — запись архива, отнесённая к региону.
type entry struct {
	name       string
	region     string // "" для корневых справочников
	kind       string // AS_ADDR_OBJ, AS_HOUSES, ...
	compressed int64
	plain      int64
}

func listEntries(zr *zip.Reader, want map[string]bool, archiveSize int64) error {
	var (
		all      []entry
		byKind   = map[string]*entry{}
		totalAll int64
	)

	for _, f := range zr.File {
		e := classify(f.Name)
		e.compressed = int64(f.CompressedSize64)
		e.plain = int64(f.UncompressedSize64)
		totalAll += e.compressed

		// Берём корневые справочники (общие для всех регионов) и запрошенные регионы.
		if e.region != "" && !want[e.region] {
			continue
		}
		all = append(all, e)

		agg, ok := byKind[e.kind]
		if !ok {
			agg = &entry{kind: e.kind}
			byKind[e.kind] = agg
		}
		agg.compressed += e.compressed
		agg.plain += e.plain
	}

	kinds := make([]*entry, 0, len(byKind))
	for _, v := range byKind {
		kinds = append(kinds, v)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i].compressed > kinds[j].compressed })

	fmt.Printf("%-34s %12s %14s\n", "СПРАВОЧНИК", "В АРХИВЕ", "РАСПАКОВАНО")
	fmt.Println(strings.Repeat("-", 64))

	var needCompressed, needPlain int64
	for _, k := range kinds {
		fmt.Printf("%-34s %12s %14s\n", k.kind, humanBytes(k.compressed), humanBytes(k.plain))
		needCompressed += k.compressed
		needPlain += k.plain
	}

	fmt.Println(strings.Repeat("-", 64))
	fmt.Printf("%-34s %12s %14s\n", fmt.Sprintf("ИТОГО (%d файлов)", len(all)),
		humanBytes(needCompressed), humanBytes(needPlain))

	fmt.Printf("\nВесь архив:            %s\n", humanBytes(archiveSize))
	fmt.Printf("Нужно скачать:         %s\n", humanBytes(needCompressed))
	fmt.Printf("Экономия против всего: %.1f%%\n",
		100*(1-float64(needCompressed)/float64(archiveSize)))
	return nil
}

// classify разбирает имя записи вида "77/AS_HOUSES_20260814_....XML".
func classify(name string) entry {
	e := entry{name: name}

	base := name
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		dir := name[:i]
		base = name[i+1:]
		// Регион — числовой каталог верхнего уровня.
		if isDigits(dir) {
			e.region = strings.TrimLeft(dir, "0")
			if e.region == "" {
				e.region = dir
			}
		}
	}

	// AS_HOUSES_20260814_uuid.XML -> AS_HOUSES
	up := strings.ToUpper(base)
	up = strings.TrimSuffix(up, ".XML")
	parts := strings.Split(up, "_")
	var kindParts []string
	for _, p := range parts {
		// Дата и UUID отбрасываются: с них начинается «хвост» имени.
		if len(p) >= 8 && isDigits(p) {
			break
		}
		kindParts = append(kindParts, p)
	}
	e.kind = strings.Join(kindParts, "_")
	if e.kind == "" {
		e.kind = up
	}
	return e
}

func latestGarURL(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("запрос к API ФНС: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var info struct {
		GarXMLFullURL string `json:"GarXMLFullURL"`
		TextVersion   string `json:"TextVersion"`
		Date          string `json:"Date"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return "", fmt.Errorf("разбор ответа API ФНС: %w", err)
	}
	if info.GarXMLFullURL == "" {
		return "", fmt.Errorf("API ФНС не вернул ссылку на полный архив")
	}
	fmt.Printf("Версия:  %s (%s)\n", info.TextVersion, info.Date)
	return info.GarXMLFullURL, nil
}

func parseRegions(s string) map[string]bool {
	m := map[string]bool{}
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			m[strings.TrimLeft(r, "0")] = true
		}
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d Б", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %s", float64(n)/float64(div), [...]string{"КБ", "МБ", "ГБ", "ТБ"}[exp])
}
