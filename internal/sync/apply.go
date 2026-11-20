package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/ingest"
	"github.com/zazzool/y-fias/internal/snapshot"
)

// ApplyStats — что изменила дельта.
type ApplyStats struct {
	Version int64

	StreetsAdded   int
	StreetsUpdated int
	StreetsRemoved int

	HousesAdded   int
	HousesUpdated int
	HousesRemoved int

	Elapsed time.Duration
	Fetched int64
}

// ApplyDelta накладывает дельту на существующий справочник.
//
// Дельта содержит ТОЛЬКО изменившиеся записи и устроена так же, как полный
// архив: та же схема XML, те же разделы по регионам. Поэтому разбор здесь
// переиспользуется целиком, а отличается лишь то, что результат не заменяет
// справочник, а сливается с ним.
//
// Объекты опознаются по OBJECTID. Именно ради этого снапшот с версии формата
// 04 хранит идентификаторы ГАР: по названию объект не узнать — оно как раз и
// могло измениться, а переименование улицы это самое важное, что дельта
// вообще приносит.
func ApplyDelta(
	ctx context.Context,
	cur snapshot.Data,
	a *gar.Archive,
	version int64,
	opt ingest.Options,
) (snapshot.Data, ApplyStats, error) {
	log := opt.Log
	if log == nil {
		log = slog.Default()
	}
	start := time.Now()
	st := ApplyStats{Version: version}

	// Текущее состояние в виде, пригодном для слияния.
	byObject := make(map[int64]index.Street, len(cur.Streets))
	for _, s := range cur.Streets {
		if s.ObjectID == 0 {
			// Снапшот собран старым ингестом без идентификаторов. Сливать
			// нечем: молча применить дельту значило бы получить справочник,
			// про который нельзя сказать, что в нём.
			return snapshot.Data{}, st, fmt.Errorf(
				"в справочнике нет идентификаторов ГАР; нужна полная пересборка")
		}
		byObject[s.ObjectID] = s
	}

	housesByObject := make(map[int64][]index.HouseRef, len(cur.Houses))
	for pos, list := range cur.Houses {
		if pos < uint32(len(cur.Streets)) {
			housesByObject[cur.Streets[pos].ObjectID] = list
		}
	}

	// Шаг 1. Изменившиеся адресные объекты.
	//
	// Читаются ВСЕ уровни: дельта могла принести и новый район, от которого
	// зависит контекст улиц.
	changed := map[int64]gar.Object{}
	removed := map[int64]bool{}

	for _, e := range a.Entries(gar.KindAddrObj, opt.Regions) {
		if err := ctx.Err(); err != nil {
			return snapshot.Data{}, st, err
		}
		_, err := streamDelta(e, func(r io.Reader) error {
			return parseDeltaObjects(r, e.Region, changed, removed)
		})
		if err != nil {
			return snapshot.Data{}, st, err
		}
	}
	log.Info("дельта: адресные объекты", "изменено", len(changed), "упразднено", len(removed))

	// Шаг 2. Изменившиеся связи иерархии.
	parents := map[int64]int64{}
	for _, kind := range []string{gar.KindMunHierarchy, gar.KindAdmHierarchy} {
		for _, e := range a.Entries(kind, opt.Regions) {
			if err := ctx.Err(); err != nil {
				return snapshot.Data{}, st, err
			}
			_, err := streamDelta(e, func(r io.Reader) error {
				_, err := gar.ParseLinks(r, gar.Filter{}, func(l gar.Link) {
					// Муниципальная иерархия читается первой и не
					// перезаписывается административной: район Москвы
					// живёт именно в ней.
					if _, ok := parents[l.ObjectID]; !ok {
						parents[l.ObjectID] = l.ParentID
					}
				})
				return err
			})
			if err != nil {
				return snapshot.Data{}, st, err
			}
		}
	}
	log.Info("дельта: связи иерархии", "изменено", len(parents))

	// Шаг 3. Слияние улиц.
	for id, o := range changed {
		prev, existed := byObject[id]

		s := index.Street{
			ObjectID: id,
			ParentID: prev.ParentID,
			Name:     o.Name,
			Type:     ingest.StreetTypeOf(o),
			Region:   o.Region,
			// Контекст сохраняется от прежней версии: дельта не содержит
			// всей цепочки родителей, и вычислить район заново не из чего.
			// Если сменился родитель, это видно по parents ниже.
			District:   prev.District,
			Settlement: prev.Settlement,
			Calls:      prev.Calls,
			Source:     prev.Source,
		}
		if p, ok := parents[id]; ok {
			s.ParentID = p
		}

		byObject[id] = s
		if existed {
			st.StreetsUpdated++
		} else {
			st.StreetsAdded++
		}
	}

	for id := range removed {
		if _, ok := byObject[id]; ok {
			delete(byObject, id)
			delete(housesByObject, id)
			st.StreetsRemoved++
		}
	}

	// Шаг 4. Дома, если они были в справочнике.
	if len(housesByObject) > 0 {
		if err := applyHouseDelta(ctx, a, opt, housesByObject, &st); err != nil {
			return snapshot.Data{}, st, err
		}
	}

	// Шаг 5. Сборка нового состояния.
	out := snapshot.Data{
		Streets: make([]index.Street, 0, len(byObject)),
	}
	for _, s := range byObject {
		out.Streets = append(out.Streets, s)
	}

	if len(housesByObject) > 0 {
		out.Houses = make(map[uint32][]index.HouseRef, len(housesByObject))
		for i, s := range out.Streets {
			if list, ok := housesByObject[s.ObjectID]; ok {
				out.Houses[uint32(i)] = list
			}
		}
	}

	st.Elapsed = time.Since(start)
	st.Fetched, _ = a.Fetched()
	return out, st, nil
}

// applyHouseDelta сливает изменения домов.
func applyHouseDelta(
	ctx context.Context,
	a *gar.Archive,
	opt ingest.Options,
	housesByObject map[int64][]index.HouseRef,
	st *ApplyStats,
) error {
	// Позиция дома: его идентификатор -> улица, где он лежит сейчас.
	streetOfHouse := make(map[int64]int64, 1<<16)
	for streetID, list := range housesByObject {
		for _, h := range list {
			streetOfHouse[h.ObjectID] = streetID
		}
	}

	// Связи «дом -> улица» для новых домов приходят в той же дельте.
	newParent := map[int64]int64{}
	for _, kind := range []string{gar.KindMunHierarchy, gar.KindAdmHierarchy} {
		for _, e := range a.Entries(kind, opt.Regions) {
			_, err := streamDelta(e, func(r io.Reader) error {
				_, err := gar.ParseLinks(r, gar.Filter{}, func(l gar.Link) {
					if _, ok := housesByObject[l.ParentID]; ok {
						newParent[l.ObjectID] = l.ParentID
					}
				})
				return err
			})
			if err != nil {
				return err
			}
		}
	}

	for _, e := range a.Entries(gar.KindHouses, opt.Regions) {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := parseDeltaHouses(e, func(h gar.House, active bool) {
			streetID, known := streetOfHouse[h.ObjectID]
			if !known {
				streetID = newParent[h.ObjectID]
			}
			if streetID == 0 {
				return // дом чужой улицы, нас не касается
			}

			list := housesByObject[streetID]

			if !active {
				for i, cur := range list {
					if cur.ObjectID == h.ObjectID {
						housesByObject[streetID] = append(list[:i], list[i+1:]...)
						st.HousesRemoved++
						return
					}
				}
				return
			}

			canon := ingest.CanonicalHouseOf(h)
			if canon == "" {
				return
			}

			for i, cur := range list {
				if cur.ObjectID == h.ObjectID {
					list[i].Canonical = canon
					list[i].Num = 0 // пересчитается при SetHouses
					st.HousesUpdated++
					return
				}
			}
			housesByObject[streetID] = append(list, index.HouseRef{
				ObjectID:  h.ObjectID,
				Canonical: canon,
			})
			st.HousesAdded++
		}, opt.Regions)
		if err != nil {
			return err
		}
		_ = e
	}
	return nil
}

/* ── Разбор дельты ─────────────────────────────────────────────── */

// parseDeltaObjects разделяет изменившиеся объекты на действующие и
// упразднённые.
//
// В полном архиве неактуальные записи просто пропускаются, здесь же они несут
// смысл: объект был и перестал быть. Пропустить их значило бы навсегда
// оставить в справочнике снесённые улицы.
func parseDeltaObjects(r io.Reader, region string, changed map[int64]gar.Object, removed map[int64]bool) error {
	return gar.ParseAll(r, func(a gar.Attrs) {
		id := a.Int64("OBJECTID")
		if id == 0 {
			return
		}
		if !a.IsCurrent() {
			// Упразднение засчитывается, только если объект не пришёл в
			// этой же дельте как действующий: ГАР присылает и старую, и
			// новую редакцию.
			if _, ok := changed[id]; !ok {
				removed[id] = true
			}
			return
		}
		delete(removed, id)
		changed[id] = gar.Object{
			ObjectID: id,
			Name:     a.Str("NAME"),
			TypeName: a.Str("TYPENAME"),
			Level:    uint8(a.Int64("LEVEL")),
			Region:   region,
		}
	})
}

func parseDeltaHouses(e gar.Entry, fn func(gar.House, bool), regions []string) error {
	rc, err := e.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	return gar.ParseAll(rc, func(a gar.Attrs) {
		id := a.Int64("OBJECTID")
		if id == 0 {
			return
		}
		fn(gar.House{
			ObjectID: id,
			Num:      a.Str("HOUSENUM"),
			Type:     uint8(a.Int64("HOUSETYPE")),
			AddNum1:  a.Str("ADDNUM1"),
			AddType1: uint8(a.Int64("ADDTYPE1")),
			AddNum2:  a.Str("ADDNUM2"),
			AddType2: uint8(a.Int64("ADDTYPE2")),
			Region:   e.Region,
		}, a.IsCurrent())
	})
}

func streamDelta(e gar.Entry, fn func(io.Reader) error) (int, error) {
	rc, err := e.Open()
	if err != nil {
		return 0, fmt.Errorf("открытие %s: %w", e.Kind, err)
	}
	defer func() { _ = rc.Close() }()
	return 0, fn(rc)
}
