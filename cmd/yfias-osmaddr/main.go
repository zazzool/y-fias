// Команда yfias-osmaddr выгружает адресные точки OpenStreetMap по областям.
//
// Одним запросом Московская область не выгружается: публичный Overpass
// отвечает 504 даже на подсчёт. Поэтому запросы идут ПО РАЙОНАМ — их
// идентификаторы уже есть в файле границ, — а результат дописывается в
// выходной файл по мере поступления.
//
// Выгрузка устроена так, чтобы её можно было прервать и продолжить: уже
// полученные районы пропускаются. Это не удобство, а необходимость — общий
// Overpass отказывает регулярно, и терять час работы из-за одного 504 нельзя.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/zazzool/y-fias/internal/geo"
)

func main() {
	var (
		geoPath = flag.String("geo", "data/geo.json", "файл границ районов")
		out     = flag.String("out", "data/osm/addr.jsonl", "куда дописывать точки")
		region  = flag.String("region", "50", "регион: 77 или 50")
		pause   = flag.Duration("pause", 5*time.Second, "пауза между запросами")
		timeout = flag.Duration("timeout", 4*time.Hour, "общий лимит времени")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	if err := run(ctx, log, *geoPath, *out, *region, *pause); err != nil {
		log.Error("выгрузка не удалась", "ошибка", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, geoPath, out, region string, pause time.Duration) error {
	areas, err := loadAreas(geoPath)
	if err != nil {
		return err
	}

	var todo []geo.Area
	for _, a := range areas {
		if a.Region == region && a.ID > 0 {
			todo = append(todo, a)
		}
	}
	if len(todo) == 0 {
		return fmt.Errorf("в %s нет районов региона %s", geoPath, region)
	}
	log.Info("районов к выгрузке", "всего", len(todo), "регион", region)

	// Уже выгруженные районы пропускаются: Overpass отказывает регулярно, и
	// начинать сначала после каждого сбоя недопустимо.
	done, err := readDone(out)
	if err != nil {
		return err
	}
	if len(done) > 0 {
		log.Info("уже выгружено", "районов", len(done))
	}

	f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	bw := bufio.NewWriterSize(f, 1<<20)
	defer func() { _ = bw.Flush() }()

	client := &http.Client{Timeout: 15 * time.Minute}
	var total, failed int

	for i, a := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		if done[a.ID] {
			continue
		}

		log.Info("район", "номер", fmt.Sprintf("%d/%d", i+1, len(todo)),
			"название", a.Name, "id", a.ID)

		points, err := fetchArea(ctx, client, a.ID)
		if err != nil {
			// Отказ на одном районе не должен останавливать выгрузку:
			// его можно добрать повторным запуском.
			log.Warn("район не выгружен, продолжаю", "название", a.Name, "причина", err)
			failed++
			continue
		}

		rec := areaRecord{AreaID: a.ID, Name: a.Name, Region: a.Region, Points: points}
		line, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if _, err := bw.Write(append(line, '\n')); err != nil {
			return err
		}
		if err := bw.Flush(); err != nil {
			return err
		}

		total += len(points)
		log.Info("район выгружен", "название", a.Name, "точек", len(points), "всего", total)

		// Пауза между запросами: общий Overpass делится со всеми, и
		// выжимать из него максимум — верный способ получить бан.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
	}

	log.Info("готово", "точек", total, "районов не выгружено", failed, "файл", out)
	if failed > 0 {
		log.Info("запустите команду повторно, чтобы добрать пропущенные районы")
	}
	return nil
}

// areaRecord — строка выходного файла: один район со своими точками.
type areaRecord struct {
	AreaID int64       `json:"area_id"`
	Name   string      `json:"name"`
	Region string      `json:"region"`
	Points []addrPoint `json:"points"`
}

type addrPoint struct {
	Lon    float64 `json:"lon"`
	Lat    float64 `json:"lat"`
	Street string  `json:"street"`
	House  string  `json:"house"`
}

func fetchArea(ctx context.Context, client *http.Client, areaID int64) ([]addrPoint, error) {
	query := fmt.Sprintf(`[out:json][timeout:600];
(
  node["addr:housenumber"]["addr:street"](area:%d);
  way["addr:housenumber"]["addr:street"](area:%d);
);
out center tags;
`, 3600000000+areaID, 3600000000+areaID)

	body, err := geo.PostOverpass(ctx, client, query)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Elements []struct {
			Lat    float64 `json:"lat"`
			Lon    float64 `json:"lon"`
			Center *struct {
				Lat float64 `json:"lat"`
				Lon float64 `json:"lon"`
			} `json:"center"`
			Tags map[string]string `json:"tags"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("разбор ответа: %w", err)
	}

	out := make([]addrPoint, 0, len(resp.Elements))
	for _, el := range resp.Elements {
		lat, lon := el.Lat, el.Lon
		if el.Center != nil {
			lat, lon = el.Center.Lat, el.Center.Lon
		}
		if lat == 0 && lon == 0 {
			continue
		}
		street, house := el.Tags["addr:street"], el.Tags["addr:housenumber"]
		if street == "" || house == "" {
			continue
		}
		out = append(out, addrPoint{Lon: lon, Lat: lat, Street: street, House: house})
	}
	return out, nil
}

// readDone собирает идентификаторы уже выгруженных районов.
func readDone(path string) (map[int64]bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return map[int64]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	done := map[int64]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 256<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec struct {
			AreaID int64 `json:"area_id"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			// Оборванная строка от прерванного запуска: район выгрузится
			// заново, ничего страшного.
			continue
		}
		done[rec.AreaID] = true
	}
	return done, sc.Err()
}

func loadAreas(path string) ([]geo.Area, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var areas []geo.Area
	if err := json.NewDecoder(f).Decode(&areas); err != nil {
		return nil, err
	}
	return areas, nil
}
