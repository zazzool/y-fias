package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/zazzool/y-fias/internal/gar"
	"github.com/zazzool/y-fias/internal/index"
	"github.com/zazzool/y-fias/internal/ingest"
	"github.com/zazzool/y-fias/internal/snapshot"
	"github.com/zazzool/y-fias/internal/store"
)

// Scheduler периодически догоняет справочник.
//
// Работает в фоне и НИКОГДА не мешает обслуживанию: пока идёт обновление,
// сервис отвечает на текущем снапшоте, а подмена происходит одним
// atomic.Pointer.Store уже после всех проверок.
//
// Отказ синхронизации — не авария. Справочник просто стареет, и это видно по
// метрике snapshot_age_seconds; отвечать при этом сервис продолжает.
type Scheduler struct {
	holder   *index.Holder
	store    *store.Store
	mis      *store.Store
	log      *slog.Logger
	client   *http.Client
	snapPath string
	regions  []string

	interval time.Duration

	trigger chan struct{}
	stop    chan struct{}
	wg      sync.WaitGroup

	mu       sync.Mutex
	lastRun  time.Time
	lastErr  error
	lastPlan Plan
	runs     int
	failures int
}

type SchedulerOptions struct {
	Holder *index.Holder

	// Store — наша база: журнал версий и очередь расхождений.
	Store *store.Store
	// MIS — база МИС, ТОЛЬКО НА ЧТЕНИЕ. Отдельное подключение, а не то же
	// самое: до этапа 5 таблица addresses принадлежит МИС, и случайная
	// запись в неё должна требовать осознанного действия.
	MIS      *store.Store
	Log      *slog.Logger
	SnapPath string
	Regions  []string

	// Interval — как часто проверять источник. ФНС публикует дважды в
	// неделю, поэтому чаще раза в сутки смысла нет: это была бы нагрузка на
	// чужой сервис без всякой пользы.
	Interval time.Duration
}

func NewScheduler(opt SchedulerOptions) *Scheduler {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	if opt.Interval <= 0 {
		opt.Interval = 12 * time.Hour
	}
	if len(opt.Regions) == 0 {
		opt.Regions = []string{"77", "50"}
	}

	return &Scheduler{
		holder:   opt.Holder,
		store:    opt.Store,
		mis:      opt.MIS,
		log:      opt.Log,
		client:   &http.Client{Timeout: 20 * time.Minute},
		snapPath: opt.SnapPath,
		regions:  opt.Regions,
		interval: opt.Interval,
		trigger:  make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}
}

// Start запускает фоновый цикл.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go s.loop()
}

// Stop останавливает цикл, дождавшись текущего прогона.
func (s *Scheduler) Stop() {
	close(s.stop)
	s.wg.Wait()
}

// Trigger просит проверить источник вне расписания. Не блокирует.
func (s *Scheduler) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default: // прогон уже запрошен
	}
}

// Status — состояние для healthz и метрик.
type Status struct {
	LastRun   time.Time
	LastError string
	Runs      int
	Failures  int
	Mode      string
	LagVer    int
}

func (s *Scheduler) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := Status{
		LastRun:  s.lastRun,
		Runs:     s.runs,
		Failures: s.failures,
		Mode:     s.lastPlan.Mode.String(),
		LagVer:   s.lastPlan.LagVersions(),
	}
	if s.lastErr != nil {
		st.LastError = s.lastErr.Error()
	}
	return st
}

func (s *Scheduler) loop() {
	defer s.wg.Done()

	// Первая проверка — сразу: если справочник устарел за время простоя,
	// узнать об этом надо при старте, а не через двенадцать часов.
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-s.trigger:
		case <-timer.C:
		}
		timer.Reset(s.interval)

		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		err := s.runOnce(ctx)
		cancel()

		s.mu.Lock()
		s.lastRun = time.Now()
		s.lastErr = err
		s.runs++
		if err != nil {
			s.failures++
		}
		s.mu.Unlock()

		if err != nil {
			// Отказ синхронизации не роняет сервис и не считается аварией:
			// справочник продолжает обслуживать запросы, просто стареет.
			s.log.Warn("синхронизация не удалась, справочник остаётся прежним",
				"ошибка", err)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) error {
	cur, hdr, err := snapshot.Read(s.snapPath)
	if err != nil {
		return fmt.Errorf("чтение справочника: %w", err)
	}

	versions, err := gar.AllVersions(ctx, s.client)
	if err != nil {
		return fmt.Errorf("список версий: %w", err)
	}

	plan, err := Build(int(hdr.Version), versions)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.lastPlan = plan
	s.mu.Unlock()

	s.log.Info("синхронизация: план",
		"режим", plan.Mode.String(),
		"с версии", plan.Current, "на версию", plan.Target,
		"дельт", plan.LagVersions(), "причина", plan.Reason)

	switch plan.Mode {
	case ModeUpToDate:
		return nil

	case ModeFull:
		// Полная пересборка занимает минуты и гигабайт трафика, поэтому
		// автоматически не запускается: это решение человека.
		s.log.Warn("требуется полная пересборка справочника",
			"причина", plan.Reason, "команда", "make snapshot")
		return nil

	case ModeDelta:
		return s.applyDeltas(ctx, cur, plan)
	}
	return nil
}

func (s *Scheduler) applyDeltas(ctx context.Context, cur snapshot.Data, plan Plan) error {
	before := len(cur.Streets)

	for _, v := range plan.Deltas {
		a, err := gar.OpenURL(ctx, s.client, v.DeltaURL)
		if err != nil {
			return fmt.Errorf("дельта %d: %w", v.VersionID, err)
		}

		next, st, err := ApplyDelta(ctx, cur, a, int64(v.VersionID),
			ingest.Options{Regions: s.regions, Log: s.log})
		_ = a.Close()
		if err != nil {
			return fmt.Errorf("дельта %d: %w", v.VersionID, err)
		}

		s.log.Info("дельта применена", "версия", v.VersionID,
			"улиц +", st.StreetsAdded, "изменено", st.StreetsUpdated,
			"удалено", st.StreetsRemoved,
			"домов +", st.HousesAdded, "изменено", st.HousesUpdated)
		cur = next
	}

	// Проверка ПЕРЕД подменой: справочник, потерявший десятую часть улиц,
	// подменять нельзя. Ошибка слияния не падает, она молча оставляет
	// неполные данные, и поймать её можно только так.
	if after := len(cur.Streets); after < before*9/10 {
		return fmt.Errorf("после дельт улиц %d вместо %d — подмена отменена",
			after, before)
	}

	newSnap := index.Build(cur.Streets, int64(plan.Target))
	newSnap.SetHouses(cur.Houses)

	if err := smokeTest(newSnap); err != nil {
		return fmt.Errorf("новый справочник не прошёл проверку: %w", err)
	}

	// Сверка с нашими адресами ДО подмены: если доля найденных резко упала,
	// дельта применилась неверно.
	report := s.reconcile(ctx, newSnap)

	// Файл пишется первым, снапшот подменяется вторым: если запись не
	// удалась, в памяти останется прежний справочник, а не новый, которого
	// нет на диске.
	backup := s.snapPath + ".prev"
	_ = os.Remove(backup)
	if err := os.Rename(s.snapPath, backup); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("сохранение предыдущего справочника: %w", err)
	}
	if _, err := snapshot.Write(s.snapPath, cur, int64(plan.Target)); err != nil {
		_ = os.Rename(backup, s.snapPath)
		return fmt.Errorf("запись справочника: %w", err)
	}

	s.holder.Store(newSnap)
	s.log.Info("справочник обновлён",
		"версия", plan.Target, "улиц", newSnap.Len(), "домов", newSnap.HouseCount())

	s.record(ctx, plan, newSnap, report)
	return nil
}

// smokeTest проверяет, что новый справочник вообще работает.
//
// Адреса подобраны так, чтобы они существовали при любом состоянии ГАР.
// Справочник, не находящий их, подменять нельзя, даже если по числу улиц он
// выглядит целым.
func smokeTest(s *index.Snapshot) error {
	known := []string{
		"Саляма Адиля улица",
		"Тверская улица",
		"Ленинский проспект",
		"Профсоюзная улица",
	}

	var missing []string
	for _, q := range known {
		if len(s.Suggest(index.Query{Text: q, Limit: 1})) == 0 {
			missing = append(missing, q)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("не находятся заведомо существующие адреса: %v", missing)
	}
	return nil
}

// reconcile сверяет справочник с нашими адресами и кладёт расхождения в
// очередь. Ошибки сверки не отменяют обновление: справочник уже проверен, а
// сверка — дополнительная польза, а не условие.
func (s *Scheduler) reconcile(ctx context.Context, newSnap *index.Snapshot) Report {
	var rep Report
	if s.store == nil {
		return rep
	}

	if s.mis == nil {
		return rep
	}
	rows, err := s.mis.OurAddresses(ctx)
	if err != nil {
		s.log.Warn("адреса МИС недоступны, сверка пропущена", "причина", err)
		return rep
	}

	ours := make([]OurAddress, 0, len(rows))
	for _, r := range rows {
		ours = append(ours, OurAddress{
			ID: r.ID, Street: r.Street, House: r.House, District: r.District,
		})
	}

	rep = Reconcile(ours, s.holder.Load(), newSnap)
	s.log.Info("сверка с адресами МИС",
		"адресов", rep.Total,
		"найдено в ГАР", rep.MatchedGar,
		"доля", fmt.Sprintf("%.1f%%", rep.MatchRate()*100),
		"расхождений", len(rep.Changes))

	// Резкое падение доли — признак неверно применённой дельты.
	if prev, ok := s.store.PreviousMatchRate(ctx); ok && rep.MatchRate() < prev-0.01 {
		s.log.Warn("доля адресов, найденных в ГАР, упала",
			"было", fmt.Sprintf("%.1f%%", prev*100),
			"стало", fmt.Sprintf("%.1f%%", rep.MatchRate()*100),
			"замечание", "проверьте справочник; откат — переименование файла .prev")
	}

	items := make([]store.ReviewItem, 0, len(rep.Changes))
	for _, c := range rep.Changes {
		items = append(items, store.ReviewItem{
			Kind:       c.Kind.String(),
			AddressIDs: c.AddressIDs,
			OldValue:   c.OldValue,
			NewValue:   c.NewValue,
			Note:       c.Note,
			GarVersion: newSnap.Version,
		})
	}
	if n, err := s.store.EnqueueReview(ctx, items); err != nil {
		s.log.Warn("расхождения не записаны в очередь", "причина", err)
	} else if n > 0 {
		s.log.Info("расхождения переданы человеку", "новых", n)
	}
	return rep
}

func (s *Scheduler) record(ctx context.Context, plan Plan, snap *index.Snapshot, rep Report) {
	if s.store == nil {
		return
	}
	err := s.store.RecordVersion(ctx, store.GarVersion{
		VersionID: int64(plan.Target),
		Mode:      "delta",
		Streets:   snap.Len(),
		Houses:    snap.HouseCount(),
		MatchRate: rep.MatchRate(),
		Note:      plan.Reason,
	})
	if err != nil {
		s.log.Warn("версия не записана в журнал", "причина", err)
	}
}

// ErrNoStore — сверка невозможна без базы.
var ErrNoStore = errors.New("хранилище не подключено")
