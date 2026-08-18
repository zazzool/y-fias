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
	"syscall"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/snapshot"
	ysync "github.com/zazzool/y-fias/internal/sync"
)

func main() {
	var (
		snapPath = flag.String("snapshot", "data/gar.yfsnap", "рабочий снапшот")
		dryRun   = flag.Bool("dry-run", false, "только показать план, ничего не делать")
		timeout  = flag.Duration("timeout", time.Hour, "общий лимит времени")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, log, *snapPath, *dryRun); err != nil {
		log.Error("синхронизация не удалась", "ошибка", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, snapPath string, dryRun bool) error {
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
		// Применение дельт к снапшоту — следующий шаг этапа 2а. Пока
		// честно сообщаем, что умеем только полную пересборку, вместо
		// того чтобы делать вид, будто применили.
		return fmt.Errorf(
			"применение дельт ещё не реализовано; соберите заново: make snapshot")

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

var _ = smokeTest
