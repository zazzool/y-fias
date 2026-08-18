// Команда yfias — адресный сервис.
//
// Справочник загружается в память при старте и обслуживается без обращений к
// сети и к базе: подсказки продолжают работать, даже когда недоступны
// интернет и PostgreSQL (§9.1 архитектуры). Это главное отличие от прежней
// архитектуры, где кэш жил в процессе с TTL в три минуты.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zazzool/y-fias/internal/api"
	"github.com/zazzool/y-fias/internal/geo"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/snapshot"
	"github.com/zazzool/y-fias/internal/store"
	ysync "github.com/zazzool/y-fias/internal/sync"
)

func main() {
	var (
		addr      = flag.String("addr", ":8080", "адрес прослушивания")
		snapPath  = flag.String("snapshot", "data/gar.yfsnap", "файл справочника")
		geoPath   = flag.String("geo", "data/geo.json", "файл границ районов")
		coordPath = flag.String("coords", "data/coords.yfcoord", "файл координат адресов")
		noHouses  = flag.Bool("no-houses", false, "не загружать дома (быстрее старт, меньше памяти)")
		dsn       = flag.String("dsn", "", "строка подключения к PostgreSQL; без неё журнал не ведётся")
		syncEvery = flag.Duration("sync", 0,
			"как часто догонять справочник дельтами; 0 — не синхронизировать")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := run(*addr, *snapPath, *geoPath, *coordPath, *dsn, *syncEvery, *noHouses, log); err != nil {
		log.Error("сервис остановлен с ошибкой", "ошибка", err)
		os.Exit(1)
	}
}

func run(addr, snapPath, geoPath, coordPath, dsn string, syncEvery time.Duration, noHouses bool, log *slog.Logger) error {
	start := time.Now()

	// Справочник загружается ДО открытия порта: сервис, принимающий запросы
	// без справочника, отвечал бы пустотой и выглядел исправным.
	holder, err := loadSnapshot(snapPath, noHouses, log)
	if err != nil {
		return err
	}

	areas, err := loadAreas(geoPath, log)
	if err != nil {
		// Границы не обязательны: без них район выводится по соседним домам,
		// как в старой системе. Отказывать в обслуживании из-за этого нельзя.
		log.Warn("границы районов не загружены, определение района по координатам недоступно",
			"причина", err)
	}

	// Координаты необязательны, как и границы: без них район выводится
	// прежним способом, по соседним домам.
	var coords *geo.Coords
	if coordPath != "" {
		c, err := geo.ReadCoords(coordPath)
		if err != nil {
			log.Warn("координаты адресов не загружены", "причина", err)
		} else {
			coords = c
			log.Info("координаты адресов загружены", "адресов", c.Len())
		}
	}

	// База НЕОБЯЗАТЕЛЬНА. Без неё сервис работает полностью — просто не
	// накапливает материал для обучения. Отказывать в подсказках из-за
	// недоступности журнала было бы прямо противоположно §9.1 архитектуры,
	// ради которого справочник и держится в памяти.
	var (
		journal *store.Journal
		st      *store.Store
	)
	if dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		opened, err := store.Open(ctx, dsn)
		cancel()

		if err != nil {
			log.Warn("база недоступна, журнал событий не ведётся", "причина", err)
		} else {
			mctx, mcancel := context.WithTimeout(context.Background(), time.Minute)
			err := opened.Migrate(mctx)
			mcancel()
			if err != nil {
				log.Warn("миграции не накатились, журнал не ведётся", "причина", err)
				opened.Close()
			} else {
				st = opened
				journal = store.NewJournal(st, store.JournalOptions{Log: log})
				defer func() {
					journal.Close()
					st.Close()
				}()
				log.Info("журнал событий включён")
			}
		}
	}

	metrics := api.NewMetrics()
	metrics.BindSnapshot(
		holder.Age,
		func() int {
			if s := holder.Load(); s != nil {
				return s.Len()
			}
			return 0
		},
	)

	if journal != nil {
		metrics.BindJournal(func() int64 {
			_, dropped := journal.Stats()
			return dropped
		})
	}

	// Планировщик синхронизации. Работает в фоне и не мешает обслуживанию:
	// пока идёт обновление, сервис отвечает на текущем справочнике.
	//
	// По умолчанию ВЫКЛЮЧЕН: обновление справочника — действие, которое
	// владелец должен включить осознанно, а не получить в наследство от
	// значения по умолчанию.
	if syncEvery > 0 {
		sched := ysync.NewScheduler(ysync.SchedulerOptions{
			Holder:   holder,
			Store:    st,
			Log:      log,
			SnapPath: snapPath,
			Interval: syncEvery,
		})
		sched.Start()
		defer sched.Stop()
		log.Info("синхронизация включена", "период", syncEvery.String())
	}

	srv := api.New(api.Options{
		Snapshots: holder,
		Areas:     areas,
		Coords:    coords,
		Log:       log,
		Metrics:   metrics,
		Journal:   journal,
		Store:     st,
	})

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Handler(),
		// Таймауты обязательны: без них медленный клиент удерживает
		// соединение неограниченно долго.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Слушатель создаётся ДО сообщения о готовности.
	//
	// Раньше «сервис слушает» печаталось перед ListenAndServe, и при занятом
	// порте в журнале оказывались подряд «слушает» и ошибка связывания —
	// сообщение попросту врало. Порядок должен быть таким, чтобы записанное
	// в журнал было правдой на момент записи.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("занять адрес %s: %w", addr, err)
	}

	log.Info("сервис слушает", "адрес", ln.Addr().String(),
		"старт занял", time.Since(start).Round(time.Millisecond).String())

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("получен сигнал, завершаюсь")
	}

	// Корректное завершение: начатые запросы доводятся до конца. В
	// диспетчерской обрыв запроса означает потерянную подсказку в момент,
	// когда человек её ждёт.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("завершение: %w", err)
	}
	log.Info("сервис остановлен")
	return nil
}

func loadSnapshot(path string, noHouses bool, log *slog.Logger) (*index.Holder, error) {
	t0 := time.Now()

	data, hdr, err := snapshot.Read(path)
	if err != nil {
		return nil, fmt.Errorf("чтение справочника %s: %w", path, err)
	}
	readTook := time.Since(t0)

	t1 := time.Now()
	snap := index.Build(data.Streets, hdr.Version)
	buildTook := time.Since(t1)

	houses := 0
	if !noHouses {
		t2 := time.Now()
		snap.SetHouses(data.Houses)
		houses = snap.HouseCount()
		log.Info("дома подключены", "домов", houses,
			"за", time.Since(t2).Round(time.Millisecond).String())
	}

	log.Info("справочник загружен",
		"версия", hdr.Version,
		"объектов", snap.Len(),
		"домов", houses,
		"чтение", readTook.Round(time.Millisecond).String(),
		"индексы", buildTook.Round(time.Millisecond).String())

	return index.NewHolder(snap), nil
}

func loadAreas(path string, log *slog.Logger) (*geo.Index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var areas []geo.Area
	if err := json.NewDecoder(f).Decode(&areas); err != nil {
		return nil, fmt.Errorf("разбор %s: %w", path, err)
	}

	ix := geo.NewIndex(areas)
	log.Info("границы районов загружены", "областей", ix.Len())
	return ix, nil
}
