# Сборка Y-FIAS.
#
# Двухэтапная: собираем в образе с Go, кладём в alpine. Итоговый образ
# содержит один статический бинарь и сертификаты — больше сервису ничего не
# нужно, потому что справочник лежит в файле, а не в системе.
#
# Данные (справочник, границы, координаты) в образ НЕ кладутся. Они меняются
# по своему расписанию и весят 52 МБ; монтировать их томом правильнее, чем
# пересобирать образ ради обновления справочника.

FROM golang:1.25-alpine AS build

WORKDIR /src

# Зависимости отдельным слоем: они меняются реже кода, и пересборка при
# правке одной строки не должна тянуть весь модуль заново.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO не нужен: весь код чистый Go, а статический бинарь запускается в alpine
# без возни с libc.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/yfias ./cmd/yfias

# Утилиты сборки данных кладутся рядом: без них справочник на сервере не
# обновить, а тянуть отдельный образ ради одной команды незачем.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/ ./cmd/yfias-ingest ./cmd/yfias-sync ./cmd/yfias-geo ./cmd/yfias-geocode

FROM alpine:3.20

# ca-certificates — для обращения к ФНС и Overpass по HTTPS.
# tzdata — чтобы время в журнале было московским, а не UTC.
RUN apk add --no-cache ca-certificates tzdata
ENV TZ=Europe/Moscow

# Сервис работает не от root: он принимает запросы, и лишние права ему ни к
# чему.
RUN adduser -D -u 10001 yfias

WORKDIR /app
COPY --from=build /out/ /app/

# Каталог данных монтируется томом.
RUN mkdir -p /data && chown yfias:yfias /data
VOLUME /data

USER yfias
EXPOSE 8080

# Порт слушается внутри сети docker и наружу НЕ публикуется: адрес вызова
# психиатрической неотложки — специальная категория персональных данных
# (152-ФЗ, ст. 10), а аутентификации у сервиса нет.
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/readyz >/dev/null || exit 1

ENTRYPOINT ["/app/yfias"]
CMD ["-addr=:8080", \
     "-snapshot=/data/gar.yfsnap", \
     "-geo=/data/geo.json", \
     "-coords=/data/coords.yfcoord"]
