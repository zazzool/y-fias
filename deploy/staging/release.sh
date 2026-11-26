#!/usr/bin/env bash
set +x
set -euo pipefail
umask 077

die() { printf '%s\n' "$*" >&2; exit 1; }

[[ $# == 1 || $# == 3 ]] ||
    die "Использование: $0 КАТАЛОГ [ОБРАЗ@sha256:… SHA256_СНИМКА]"
stage=$(realpath -e -- "$1")
[[ "$stage" != / && -f "$stage/repository" ]] ||
    die "Каталог стенда должен содержать файл repository"
source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
read -r repository < "$stage/repository"
[[ "$repository" =~ ^[a-z0-9.-]+(:[0-9]+)?/[a-z0-9._/-]+$ ]] ||
    die "Некорректный адрес репозитория образов"
exec 9>"$stage/release.lock"
flock -n 9 || die "Выкатка уже идёт"

compose() {
    YFIAS_STAGE="$stage" YFIAS_IMAGE="$image" YFIAS_SNAPSHOT="$snapshot" \
        docker compose --env-file /dev/null --project-name yfias-release-pilot \
        --project-directory "$source_dir" -f "$source_dir/compose.yml" "$@"
}

select_release() {
    local digest=${1#"$repository@"}
    [[ "$1" == "$repository@"* && "$digest" =~ ^sha256:[0-9a-f]{64}$ &&
        "$2" =~ ^[0-9a-f]{64}$ ]] || return 1
    image=$1
    snapshot="$stage/snapshots/$2.yfsnap"
    [[ -f "$snapshot" && ! -L "$snapshot" ]] || return 1
    printf '%s  %s\n' "$2" "$snapshot" | sha256sum --status -c -
}

run_release() {
    local container pid
    compose up -d --wait --wait-timeout 35 yfias || return 1
    container=$(compose ps -q yfias) || return 1
    pid=$(docker inspect -f '{{.State.Pid}}' "$container") || return 1
    [[ "$pid" =~ ^[1-9][0-9]*$ ]] || return 1
    nsenter --target "$pid" --net -- python3 "$source_dir/probe.py" "$stage/secrets"
}

recover() {
    [[ -f "$stage/pending" ]] || return 0
    if [[ -f "$stage/current" ]]; then
        local previous_image previous_snapshot
        read -r previous_image previous_snapshot < "$stage/current" || return 1
        select_release "$previous_image" "$previous_snapshot" || return 1
        # Образ принятой версии остаётся в локальном кэше на случай отказа реестра.
        run_release || return 1
        printf 'Восстановлена принятая версия %s\n' "$previous_image"
    else
        local pending_image pending_snapshot
        read -r pending_image pending_snapshot < "$stage/pending" || return 1
        # Удаление контейнера первой попытки возможно даже после потери снимка.
        image=$pending_image
        snapshot="$stage/snapshots/$pending_snapshot.yfsnap"
        compose rm -sf yfias || return 1
    fi
    rm -- "$stage/pending"
}

write_state() {
    local target=$1
    printf '%s %s\n' "$2" "$3" > "$target.tmp"
    mv -f -- "$target.tmp" "$target"
}

recover || die "Автовосстановление не прошло. Проверьте стенд и повторите восстановление"
[[ $# == 3 ]] || exit 0

select_release "$2" "$3" || die "Образ должен быть из разрешённого репозитория, снимок должен совпадать с SHA-256"
for file in postgres-password yfias-dsn yfias-keys probe-client probe-admin; do
    [[ -s "$stage/secrets/$file" ]] || die "Отсутствует стендовый секрет $file"
done
docker pull "$image"
write_state "$stage/pending" "$2" "$3"
if run_release; then
    write_state "$stage/current" "$2" "$3"
    rm -- "$stage/pending"
    printf 'Принята версия %s\n' "$2"
else
    recover || die "Кандидат отклонён, предыдущая версия не восстановилась. Состояние сохранено"
    die "Кандидат отклонён"
fi
