#!/usr/bin/env bash
set +x
set -euo pipefail
umask 077

[[ $# == 1 && "$1" =~ ^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+$ ]] ||
    { printf 'Использование: %s РЕПОЗИТОРИЙ_ОБРАЗОВ\n' "$0" >&2; exit 1; }
repository=$1
cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.."
revision=$(git rev-parse HEAD)
source=$(mktemp -d)
trap 'rm -rf -- "$source"' EXIT
mkdir -- "$source/context"
git archive "$revision" | tar -x -C "$source/context"

# Контекст состоит только из файлов коммита. Локальные .env, данные и
# результаты предыдущих сборок не могут попасть в образ.
docker buildx build --platform linux/amd64 --push \
    --tag "$repository:git-$revision" \
    --label "org.opencontainers.image.revision=$revision" \
    --label "org.opencontainers.image.source=https://github.com/zazzool/y-fias" \
    --metadata-file "$source/metadata.json" "$source/context" >&2
digest=$(jq -er '."containerimage.digest" | select(test("^sha256:[0-9a-f]{64}$"))' "$source/metadata.json")
printf '%s@%s\n' "$repository" "$digest"
