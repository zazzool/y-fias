// Package store — хранилище сервиса на PostgreSQL.
//
// Важно, чего здесь НЕТ: справочника. Подсказки обслуживаются снапшотом в
// памяти, поэтому недоступность базы не мешает искать адреса (§9.1
// архитектуры). База нужна для журнала событий, очередей и кэша — то есть для
// того, что переживает перезапуск и разделяется между экземплярами.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store — доступ к базе.
type Store struct {
	pool *pgxpool.Pool
}

// Open создаёт пул соединений.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// Ошибка pgx может включать исходную строку вместе с паролем.
		return nil, errors.New("некорректная строка подключения")
	}

	// Пул небольшой намеренно: на горячем пути базы нет, а фоновые задачи
	// (журнал, очереди) не требуют многого. Большой пул на слабой базе
	// приводит к очередям внутри PostgreSQL вместо очередей у нас.
	cfg.MaxConns = 8
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 10 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("подключение: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("проверка соединения: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// Pool — для запросов, которым нужен прямой доступ.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Migrate накатывает миграции.
//
// Миграции встроены в бинарь и применяются при старте. В прежней системе их
// накатывали руками, и 17.08.2026 выяснилось, что две числились применёнными,
// не будучи ими (§2.5 описи). Встроенные миграции с журналом в базе делают
// такое расхождение невозможным.
func (s *Store) Migrate(ctx context.Context) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	goose.SetLogger(goose.NopLogger())

	db := stdlib.OpenDBFromPool(s.pool)
	defer func() { _ = db.Close() }()

	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("накат миграций: %w", err)
	}
	return nil
}

// Version — текущая версия схемы.
func (s *Store) Version(ctx context.Context) (int64, error) {
	db := stdlib.OpenDBFromPool(s.pool)
	defer func() { _ = db.Close() }()
	return goose.GetDBVersionContext(ctx, db)
}
