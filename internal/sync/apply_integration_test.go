//go:build integration

package sync

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
	"github.com/zazzool/y-fias/internal/ingest"
	"github.com/zazzool/y-fias/internal/snapshot"
)

// TestLive_ApplyRealDelta применяет НАСТОЯЩУЮ дельту ФНС к собранному
// справочнику.
//
// Проверяется не «изменилось ли что-то» — на актуальном справочнике дельта
// той же версии почти ничего не меняет, — а то, что разбор и слияние не
// ломают справочник: улицы не пропадают, дома не теряются, идентификаторы
// сохраняются. Именно это и опасно: ошибка слияния не падает, она молча
// оставляет неполный справочник.
func TestLive_ApplyRealDelta(t *testing.T) {
	snapPath := filepath.Join("..", "..", "data", "gar.yfsnap")
	if _, err := os.Stat(snapPath); err != nil {
		t.Skipf("справочник не собран (%v). Собрать: make snapshot", err)
	}

	cur, hdr, err := snapshot.Read(snapPath)
	if err != nil {
		t.Fatalf("чтение справочника: %v", err)
	}
	t.Logf("справочник: версия %d, улиц %d, улиц с домами %d",
		hdr.Version, len(cur.Streets), len(cur.Houses))

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	client := &http.Client{Timeout: 10 * time.Minute}
	versions, err := gar.AllVersions(ctx, client)
	if err != nil {
		t.Skipf("источник недоступен: %v", err)
	}

	// Берём дельту последней публикации. Если справочник уже на ней, дельта
	// окажется почти пустой — это нормально: проверяется целостность
	// слияния, а не факт изменений.
	var deltaURL string
	var deltaVer int
	for _, v := range versions {
		if v.VersionID > deltaVer && v.DeltaURL != "" {
			deltaVer, deltaURL = v.VersionID, v.DeltaURL
		}
	}
	if deltaURL == "" {
		t.Skip("дельт нет")
	}
	t.Logf("применяю дельту версии %d", deltaVer)

	a, err := gar.OpenURL(ctx, client, deltaURL)
	if err != nil {
		t.Skipf("дельта недоступна: %v", err)
	}
	defer a.Close()

	next, st, err := ApplyDelta(ctx, cur, a, int64(deltaVer),
		ingest.Options{Regions: []string{"77", "50"}})
	if err != nil {
		t.Fatalf("применение дельты: %v", err)
	}

	t.Logf("улиц: +%d изменено %d удалено %d",
		st.StreetsAdded, st.StreetsUpdated, st.StreetsRemoved)
	t.Logf("домов: +%d изменено %d удалено %d",
		st.HousesAdded, st.HousesUpdated, st.HousesRemoved)
	t.Logf("скачано %d КБ за %s", st.Fetched/1024, st.Elapsed.Round(time.Second))

	// Дельта по двум регионам должна стоить сотни килобайт, а не гигабайты.
	// Если она вдруг выросла на порядки, значит читается лишнее, и весь
	// смысл дельта-синхронизации теряется.
	if st.Fetched > 200<<20 {
		t.Errorf("скачано %d МБ — дельта не должна стоить столько",
			st.Fetched>>20)
	}

	// Целостность: справочник не должен усохнуть.
	if len(next.Streets) < len(cur.Streets)*9/10 {
		t.Fatalf("после дельты улиц %d вместо %d — слияние потеряло данные",
			len(next.Streets), len(cur.Streets))
	}

	// Идентификаторы обязаны сохраниться: без них следующая дельта не
	// применится.
	var noID int
	for _, s := range next.Streets {
		if s.ObjectID == 0 {
			noID++
		}
	}
	if noID > 0 {
		t.Errorf("у %d улиц потерян идентификатор ГАР", noID)
	}

	// Дома должны остаться, если были.
	if len(cur.Houses) > 0 && len(next.Houses) < len(cur.Houses)*9/10 {
		t.Errorf("улиц с домами %d вместо %d — дома потерялись",
			len(next.Houses), len(cur.Houses))
	}

	t.Logf("после дельты: улиц %d, улиц с домами %d",
		len(next.Streets), len(next.Houses))
}

// Снапшот без идентификаторов ГАР нельзя обновлять дельтой: объект не
// опознать. Ошибка должна быть явной, а не молчаливой порчей справочника.
func TestApplyDeltaRefusesSnapshotWithoutIDs(t *testing.T) {
	cur, _, err := snapshot.Read(filepath.Join("..", "..", "data", "gar.yfsnap"))
	if err != nil {
		t.Skipf("справочник не собран (%v)", err)
	}
	if len(cur.Streets) == 0 {
		t.Skip("справочник пуст")
	}

	// Стираем идентификаторы, имитируя снапшот старого формата.
	for i := range cur.Streets {
		cur.Streets[i].ObjectID = 0
	}

	_, _, err = ApplyDelta(context.Background(), cur, nil, 1, ingest.Options{})
	if err == nil {
		t.Fatal("ожидалась ошибка: без идентификаторов слияние невозможно")
	}
	t.Logf("ошибка (ожидаемо): %v", err)
}
