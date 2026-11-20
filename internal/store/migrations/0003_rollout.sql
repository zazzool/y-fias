-- +goose Up
-- Флаги переключения чтения (этап 4).
--
-- Текущая доля хранится отдельно от журнала изменений: первую читают при
-- каждом старте, второй — когда разбирают, что и когда раскатывали.
-- Условие выхода этапа — «неделя на 100% без отката» — проверяется по
-- журналу, а не по памяти администратора.
CREATE TABLE IF NOT EXISTS rollout_flags (
    endpoint    text        PRIMARY KEY,
    percent     smallint    NOT NULL CHECK (percent BETWEEN 0 AND 100),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  text
);

CREATE TABLE IF NOT EXISTS rollout_changes (
    id            bigserial   PRIMARY KEY,
    ts            timestamptz NOT NULL DEFAULT now(),
    endpoint      text        NOT NULL,
    from_percent  smallint    NOT NULL,
    to_percent    smallint    NOT NULL,
    reason        text,
    actor         text
);

CREATE INDEX IF NOT EXISTS idx_rollout_changes_ts ON rollout_changes (ts DESC);

-- +goose Down
DROP TABLE IF EXISTS rollout_changes;
DROP TABLE IF EXISTS rollout_flags;
