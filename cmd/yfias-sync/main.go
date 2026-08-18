// Команда yfias-sync поддерживает справочник в актуальном состоянии.
//
// Дельта по нашим регионам стоит около 135 КБ против 53 ГБ полного архива,
// а публикации выходят дважды в неделю. Стоимость поддержания актуальности —
// примерно 14 МБ в год.
//
// Новый снапшот собирается В СТОРОНЕ и подменяет рабочий только после
// проверки: сервис не должен подняться на полусобранном справочнике.
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
	ysync "github.com/zazzool/y-fias/internal/sync"
)

func main() {
	var (
		snapPath = flag.String("snapshot", "data/gar.yfsnap", "рабочий снапшот")
		regions  = flag.String("regions", "77,50", "коды регионов через запятую")
		dryRun   = flag.Bool("dry-run", false, "только показать план, ничего не делать")
		timeout  = flag.Duration("timeout", time.Hour, "общий лимит времени")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, log, *snapPath, *regions, *dryRun); err != nil {
		log.Error("синхронизация не удалась", "ошибка", err)
		os.Exit(1)
	}
}

// applyDeltas применяет дельты по порядку и подменяет снапшот.
//
// Новый файл собирается В СТОРОНЕ и заменяет рабочий только после проверки:
// сервис не должен подняться на полусобранном справочнике. Предыдущий файл
// сохраняется — откат есть замена файла (§11.3 архитектуры).
func applyDeltas(
	ctx context.Context,
	log *slog.Logger,
	client *http.Client,
	snapPath string,
	plan ysync.Plan,
	regions []string,
) error {
	cur, hdr, err := snapshot.Read(snapPath)
	if err != nil {
		return fmt.Errorf("чтение справочника: %w", err)
	}

	before := len(cur.Streets)
	total := ysync.ApplyStats{}

	for _, v := range plan.Deltas {
		log.Info("применяю дельту", "версия", v.VersionID, "дата", v.Date)

		a, err := gar.OpenURL(ctx, client, v.DeltaURL)
		if err != nil {
			return fmt.Errorf("дельта %d: %w", v.VersionID, err)
		}

		next, st, err := ysync.ApplyDelta(ctx, cur, a, int64(v.VersionID),
			ingest.Options{Regions: regions, Log: log})
		a.Close()
		if err != nil {
			return fmt.Errorf("дельта %d: %w", v.VersionID, err)
		}

		log.Info("дельта применена",
			"версия", v.VersionID,
			"улиц +", st.StreetsAdded, "изменено", st.StreetsUpdated, "удалено", st.StreetsRemoved,
			"домов +", st.HousesAdded, "изменено", st.HousesUpdated, "удалено", st.HousesRemoved,
			"скачано", humanBytes(st.Fetched),
			"за", st.Elapsed.Round(time.Second).String())

		cur = next
		total.StreetsAdded += st.StreetsAdded
		total.StreetsUpdated += st.StreetsUpdated
		total.StreetsRemoved += st.StreetsRemoved
		total.HousesAdded += st.HousesAdded
		total.HousesUpdated += st.HousesUpdated
		total.HousesRemoved += st.HousesRemoved
		total.Fetched += st.Fetched
	}

	// Проверка ПЕРЕД подменой. Справочник, потерявший заметную часть улиц,
	// подменять нельзя: скорее всего дельта применилась неверно.
	after := len(cur.Streets)
	if after < before*9/10 {
		return fmt.Errorf(
			"после дельт улиц %d вместо %d — потеряно больше десятой части, подмена отменена",
			after, before)
	}

	snap := index.Build(cur.Streets, int64(plan.Target))
	if err := smokeTest(snap, knownAddresses); err != nil {
		return fmt.Errorf("новый справочник не прошёл проверку: %w", err)
	}

	// Предыдущий файл сохраняется рядом: откат — переименование.
	backup := snapPath + ".prev"
	if err := os.Rename(snapPath, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("сохранение предыдущего справочника: %w", err)
	}

	newHdr, err := snapshot.Write(snapPath, cur, int64(plan.Target))
	if err != nil {
		// Возвращаем прежний файл: остаться без справочника хуже, чем
		// остаться со старым.
		_ = os.Rename(backup, snapPath)
		return fmt.Errorf("запись справочника: %w", err)
	}

	log.Info("справочник обновлён",
		"версия", newHdr.Version, "было", hdr.Version,
		"улиц", after, "домов", newHdr.Houses,
		"улиц +", total.StreetsAdded, "изменено", total.StreetsUpdated,
		"удалено", total.StreetsRemoved,
		"скачано всего", humanBytes(total.Fetched),
		"предыдущий", backup)
	return nil
}

// knownAddresses — адреса, которые обязаны находиться в любом исправном
// справочнике. Проверка перед подменой файла.
var knownAddresses = []string{
	"Саляма Адиля улица",
	"Тверская улица",
	"Ленинский проспект",
	"Профсоюзная улица",
	"Ломоносовский проспект",
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

func run(ctx context.Context, log *slog.Logger, snapPath, regionList string, dryRun bool) error {
	client := &http.Client{Timeout: 15 * time.Minute}

	// Текущая версия — из заголовка снапшота. Отдельного файла состояния
	// нет намеренно: он мог бы разойтись с самим снапшотом.
	current := 0
	if _, hdr, err := snapshot.Read(snapPath); err == nil {
		current = int(hdr.Version)
		log.Info("текущий справочник", "версия", current, "собран", hdr.BuiltAt.Format(time.RFC3339))
	} else {
		log.Warn("снапшот недоступен, потребуется полная сборка", "причина", err)
	}

	versions, err := gar.AllVersions(ctx, client)
	if err != nil {
		return fmt.Errorf("список версий: %w", err)
	}
	log.Info("источник", "версий доступно", len(versions))

	plan, err := ysync.Build(current, versions)
	if err != nil {
		return err
	}

	log.Info("план",
		"режим", plan.Mode.String(),
		"с версии", plan.Current,
		"на версию", plan.Target,
		"дельт", plan.LagVersions(),
		"причина", plan.Reason)

	// Метрика §11.6: отставание в публикациях. Тревога при отставании
	// больше четырёх (примерно две недели).
	if plan.LagVersions() > 4 {
		log.Warn("справочник заметно отстал",
			"публикаций", plan.LagVersions(),
			"замечание", "проверьте, работает ли синхронизация по расписанию")
	}

	switch plan.Mode {
	case ysync.ModeUpToDate:
		log.Info("обновление не требуется")
		return nil

	case ysync.ModeDelta:
		for _, v := range plan.Deltas {
			log.Info("дельта к применению", "версия", v.VersionID, "дата", v.Date)
		}
		if dryRun {
			log.Info("сухой прогон: дельты не применяются")
			return nil
		}
		return applyDeltas(ctx, log, client, snapPath, plan, splitRegions(regionList))

	case ysync.ModeFull:
		if dryRun {
			log.Info("сухой прогон: полная пересборка не выполняется",
				"архив", plan.FullURL)
			return nil
		}
		return fmt.Errorf(
			"нужна полная пересборка (%s); выполните: make snapshot", plan.Reason)
	}

	return nil
}

// проверкаСнапшота — smoke-тест перед подменой рабочего файла.
//
// Оставлено на виду и вызывается из шага применения дельт: снапшот, который
// не находит два десятка заведомо существующих адресов, подменять нельзя.
func smokeTest(snap *index.Snapshot, known []string) error {
	var missing int
	for _, s := range known {
		if len(snap.Suggest(index.Query{Text: s, Limit: 1})) == 0 {
			missing++
		}
	}
	if missing > len(known)/10 {
		return fmt.Errorf("новый снапшот не находит %d из %d известных адресов",
			missing, len(known))
	}
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
