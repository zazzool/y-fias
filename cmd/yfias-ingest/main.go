// Команда yfias-ingest собирает поисковый снапшот из выгрузки ГАР.
//
// Архив не скачивается целиком: из 53 ГБ читаются только нужные записи через
// HTTP Range (см. internal/garzip). Результат — один файл, который сервис
// читает при старте. Деплой сводится к копированию файла, откат — к замене
// его предыдущим.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/ingest"
	"github.com/zazzool/y-fias/internal/snapshot"
)

func main() {
	var (
		out     = flag.String("out", "data/gar.yfsnap", "куда положить снапшот")
		regions = flag.String("regions", "77,50", "коды регионов через запятую")
		archive = flag.String("archive", "", "путь к скачанному gar_xml.zip; по умолчанию читается по сети")
		houses  = flag.Bool("houses", false, "загрузить дома (дольше и больше памяти)")
		timeout = flag.Duration("timeout", 2*time.Hour, "общий лимит времени")
		verbose = flag.Bool("v", false, "подробный лог")
	)
	flag.Parse()

	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, log, *out, *regions, *archive, *houses); err != nil {
		log.Error("ингест не удался", "ошибка", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, out, regions, archivePath string, withHouses bool) error {
	if err := os.MkdirAll(dir(out), 0o755); err != nil {
		return err
	}

	client := &http.Client{Timeout: 15 * time.Minute}

	var (
		a       *gar.Archive
		version int64
		err     error
	)

	if archivePath != "" {
		log.Info("открываю локальный архив", "путь", archivePath)
		a, err = gar.OpenFile(archivePath)
		version = time.Now().Unix()
	} else {
		var v gar.VersionInfo
		v, err = gar.LatestVersion(ctx, client)
		if err != nil {
			return err
		}
		log.Info("версия ГАР", "номер", v.VersionID, "дата", v.Date)
		version = int64(v.VersionID)
		a, err = gar.OpenURL(ctx, client, v.FullURL)
	}
	if err != nil {
		return err
	}
	defer func() { _ = a.Close() }()

	res, err := ingest.Run(ctx, a, version, ingest.Options{
		Regions:    splitRegions(regions),
		Log:        log,
		WithHouses: withHouses,
	})
	if err != nil {
		return err
	}

	hdr, err := snapshot.Write(out, snapshot.Data{
		Streets: res.Streets,
		Houses:  res.Houses,
	}, res.Version)
	if err != nil {
		return fmt.Errorf("запись снапшота: %w", err)
	}

	st, _ := os.Stat(out)
	log.Info("снапшот записан",
		"файл", out,
		"улиц", hdr.Streets,
		"домов", hdr.Houses,
		"размер", humanBytes(st.Size()),
		"версия", hdr.Version)

	// Проверка на месте: снапшот, который не читается обратно, бесполезен,
	// и узнать об этом надо здесь, а не при старте сервиса.
	back, _, err := snapshot.Read(out)
	if err != nil {
		return fmt.Errorf("проверочное чтение: %w", err)
	}
	if len(back.Streets) != len(res.Streets) {
		return fmt.Errorf("проверочное чтение: улиц %d вместо %d",
			len(back.Streets), len(res.Streets))
	}
	if len(back.Houses) != len(res.Houses) {
		return fmt.Errorf("проверочное чтение: улиц с домами %d вместо %d",
			len(back.Houses), len(res.Houses))
	}

	buildStart := time.Now()
	snap := index.Build(back.Streets, res.Version)
	log.Info("индекс собран",
		"улиц", snap.Len(),
		"за", time.Since(buildStart).Round(time.Millisecond))

	log.Info("готово",
		"всего", res.Elapsed.Round(time.Second),
		"скачано", humanBytes(res.Fetched),
		"из", humanBytes(a.Size()))
	return nil
}

func splitRegions(s string) []string {
	var out []string
	for _, r := range strings.Split(s, ",") {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return out
}

func dir(path string) string {
	if i := strings.LastIndexAny(path, `/\`); i > 0 {
		return path[:i]
	}
	return "."
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
