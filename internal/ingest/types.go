package ingest

import "github.com/zazzool/y-fias/internal/normalize"

// splitType — тонкая обёртка, чтобы пакет ingest не зависел от деталей
// нормализации напрямую и её можно было подменить в тестах.
func splitType(s string) (name, typ string) { return normalize.SplitStreetType(s) }
