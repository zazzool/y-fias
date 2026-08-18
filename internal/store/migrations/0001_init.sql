-- +goose Up
-- Начальная схема Y-FIAS.
--
-- Сервис владеет ТОЛЬКО своими таблицами. Таблица addresses принадлежит МИС и
-- на этапах 3-4 читается сервисом лишь на чтение; переход владения — этап 5.

-- ── Журнал событий ─────────────────────────────────────────────
--
-- Основа самообучения (§8 архитектуры). Каждое взаимодействие с адресной
-- строкой попадает сюда: что печатал человек, что мы показали, что он выбрал,
-- что осталось в карте.
--
-- Это одновременно обучающая выборка, материал для разбора аварий и основа
-- сравнения весов ранжирования. Сырой ввод хранится вечно и не переписывается,
-- поэтому любую версию модели можно переобучить с нуля.
CREATE TABLE IF NOT EXISTS addr_events (
    id            bigserial PRIMARY KEY,
    ts            timestamptz NOT NULL DEFAULT now(),
    kind          text        NOT NULL,
    session_id    uuid,

    raw_input     text,
    suggestions   jsonb,
    chosen_key    text,
    final_street  text,
    final_house   text,

    -- Версии, при которых событие произошло. Без них откат весов не
    -- откатывает выученные на них правила (§8.3 архитектуры).
    snapshot_ver  bigint,
    weights_ver   integer,

    took_ms       double precision,
    meta          jsonb
);

CREATE INDEX IF NOT EXISTS idx_addr_events_ts   ON addr_events (ts DESC);
CREATE INDEX IF NOT EXISTS idx_addr_events_kind ON addr_events (kind, ts DESC);

-- ── Версии справочника ─────────────────────────────────────────
--
-- Журнал применённых публикаций ГАР. Отдельного файла состояния нет
-- намеренно: он мог бы разойтись с самим снапшотом.
CREATE TABLE IF NOT EXISTS gar_versions (
    version_id   bigint PRIMARY KEY,
    applied_at   timestamptz NOT NULL DEFAULT now(),
    mode         text        NOT NULL,   -- delta | full
    streets      integer,
    houses       integer,
    checksum     text,
    match_rate   double precision,       -- доля наших адресов, найденных в ГАР
    note         text
);

-- ── Очередь расхождений для человека ───────────────────────────
--
-- Синхронизация НЕ переписывает наши адреса: переименование улицы в ГАР
-- создаёт запись здесь, и решение принимает диспетчер или администратор
-- (§11.4 архитектуры). Инвариант 1 действует и при обновлении справочника.
CREATE TABLE IF NOT EXISTS address_review_queue (
    id           bigserial PRIMARY KEY,
    created_at   timestamptz NOT NULL DEFAULT now(),
    kind         text        NOT NULL,   -- renamed | retired | district
    address_ids  text[]      NOT NULL,
    old_value    text,
    new_value    text,
    note         text,
    gar_version  bigint,

    resolved_at  timestamptz,
    resolved_by  text,
    resolution   text                    -- applied | rejected | postponed
);

CREATE INDEX IF NOT EXISTS idx_review_pending
    ON address_review_queue (created_at DESC)
    WHERE resolved_at IS NULL;

-- ── Веса ранжирования ──────────────────────────────────────────
--
-- Веса — значение с версией, а не константы в коде: они подбираются офлайн
-- на golden-корпусе, и версия попадает в журнал событий.
CREATE TABLE IF NOT EXISTS rank_weights (
    version      integer PRIMARY KEY,
    created_at   timestamptz NOT NULL DEFAULT now(),
    weights      jsonb   NOT NULL,
    metric       double precision,       -- букв до подсказки, по вызовам
    active       boolean NOT NULL DEFAULT false,
    note         text
);

-- Активной может быть только одна версия весов: иначе непонятно, на каких
-- весах получены события в журнале.
CREATE UNIQUE INDEX IF NOT EXISTS idx_rank_weights_active
    ON rank_weights (active) WHERE active;

-- ── Кэш ответов внешних служб ──────────────────────────────────
--
-- В базе, а не в памяти процесса. Прежние кэши жили в процессе с TTL 3-5
-- минут, не переживали перезапуск и не разделялись между экземплярами
-- (§8.7 описи).
CREATE TABLE IF NOT EXISTS upstream_cache (
    source      text        NOT NULL,
    cache_key   text        NOT NULL,
    response    jsonb       NOT NULL,
    fetched_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (source, cache_key)
);

CREATE INDEX IF NOT EXISTS idx_upstream_cache_fetched
    ON upstream_cache (fetched_at);

-- ── Очередь обогащения ─────────────────────────────────────────
--
-- Таблица, а не канал в памяти: переживает перезапуск и не теряет задания.
-- Обработка через FOR UPDATE SKIP LOCKED (§9.2 архитектуры).
CREATE TABLE IF NOT EXISTS enrich_queue (
    id          bigserial PRIMARY KEY,
    created_at  timestamptz NOT NULL DEFAULT now(),
    address_id  text,
    street      text NOT NULL,
    house       text,
    attempts    integer NOT NULL DEFAULT 0,
    last_error  text,
    taken_at    timestamptz,
    done_at     timestamptz
);

CREATE INDEX IF NOT EXISTS idx_enrich_pending
    ON enrich_queue (created_at)
    WHERE done_at IS NULL;

-- +goose Down
DROP TABLE IF EXISTS enrich_queue;
DROP TABLE IF EXISTS upstream_cache;
DROP TABLE IF EXISTS rank_weights;
DROP TABLE IF EXISTS address_review_queue;
DROP TABLE IF EXISTS gar_versions;
DROP TABLE IF EXISTS addr_events;
