// Команда yfias-geo загружает границы районов из OpenStreetMap.
//
// Запускается ОФЛАЙН и редко: границы меняются на порядки реже адресов, а
// смена границы района — это смена зоны ответственности ПНД, и она не должна
// происходить незаметно (§11.5 архитектуры). Результат кладётся в файл, и
// подмена его — осознанное действие человека.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/zazzool/y-fias/internal/geo"
)

func main() {
	var (
		out     = flag.String("out", "data/geo.json", "куда положить границы")
		regions = flag.String("regions", "77", "коды регионов: 77, 50 или 77,50")
		timeout = flag.Duration("timeout", 30*time.Minute, "общий лимит времени")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, log, *out, *regions); err != nil {
		log.Error("загрузка границ не удалась", "ошибка", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, out, regions string) error {
	client := &http.Client{Timeout: 15 * time.Minute}

	sources := []struct {
		region string
		areaID int64
		levels []int
	}{
		// Москва: районы — admin_level 8.
		{"77", geo.AreaMoscow, []int{8}},
		// Область: городские округа и поселения — 6 и 8.
		{"50", geo.AreaMO, []int{6, 8}},
	}

	var all []geo.Area
	for _, src := range sources {
		if !wanted(regions, src.region) {
			continue
		}

		log.Info("запрашиваю границы", "регион", src.region, "уровни", src.levels)
		start := time.Now()

		areas, err := geo.Fetch(ctx, client, src.areaID, src.levels, src.region)
		if err != nil {
			return fmt.Errorf("регион %s: %w", src.region, err)
		}

		var points int
		for _, a := range areas {
			for _, p := range a.Polygons {
				points += len(p.Outer)
				for _, h := range p.Inner {
					points += len(h)
				}
			}
		}
		log.Info("границы получены",
			"регион", src.region,
			"областей", len(areas),
			"точек", points,
			"за", time.Since(start).Round(time.Second))

		all = append(all, areas...)
	}

	if len(all) == 0 {
		return fmt.Errorf("не получено ни одной области")
	}

	// Проверка на месте: индекс, который не находит собственные области,
	// бесполезен, и узнать об этом надо здесь.
	ix := geo.NewIndex(all)
	if err := selfCheck(log, ix); err != nil {
		return err
	}

	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	enc.SetIndent("", " ")
	if err := enc.Encode(all); err != nil {
		return fmt.Errorf("запись %s: %w", out, err)
	}

	st, _ := f.Stat()
	log.Info("границы записаны", "файл", out, "областей", len(all),
		"размер", fmt.Sprintf("%.2f МБ", float64(st.Size())/(1<<20)))
	return nil
}

// selfCheck проверяет геометрию на известных точках.
//
// Ошибка сборки колец не приводит к падению: полигон просто оказывается
// вывернутым, и проверка попадания начинает молча врать. Поэтому проверка
// делается на точках, ответ для которых известен заранее.
func selfCheck(log *slog.Logger, ix *geo.Index) error {
	checks := []struct {
		name string
		pt   geo.Point
		want string
	}{
		{"Красная площадь", geo.Point{Lon: 37.6208, Lat: 55.7539}, "Тверской"},
		{"ГКБ 67 (Саляма Адиля)", geo.Point{Lon: 37.4906, Lat: 55.7761}, "Хорошёво-Мнёвники"},
		{"Останкинская башня", geo.Point{Lon: 37.6117, Lat: 55.8197}, "Останкинский"},
	}

	var bad int
	for _, c := range checks {
		area, n := ix.Locate(c.pt)
		switch {
		case area == nil:
			log.Error("контрольная точка не попала ни в одну область",
				"точка", c.name, "ожидался", c.want)
			bad++
		case !sameName(area.Name, c.want):
			log.Error("контрольная точка попала не туда",
				"точка", c.name, "получено", area.Name, "ожидался", c.want)
			bad++
		default:
			extra := ""
			if n > 1 {
				extra = fmt.Sprintf(" (областей подошло %d — границы перекрываются)", n)
			}
			log.Info("контрольная точка"+extra, "точка", c.name, "район", area.Name)
		}
	}

	if bad > 0 {
		return fmt.Errorf("не сошлись %d контрольных точек: геометрия собрана неверно", bad)
	}
	return nil
}

// sameName сравнивает названия районов без учёта ё/е и регистра.
func sameName(a, b string) bool {
	norm := func(s string) string {
		out := make([]rune, 0, len(s))
		for _, r := range s {
			switch r {
			case 'ё':
				r = 'е'
			case 'Ё':
				r = 'Е'
			}
			if r >= 'А' && r <= 'Я' {
				r += 'а' - 'А'
			}
			out = append(out, r)
		}
		return string(out)
	}
	return norm(a) == norm(b)
}

func wanted(list, region string) bool {
	for _, r := range splitComma(list) {
		if r == region {
			return true
		}
	}
	return false
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' || r == ' ' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
