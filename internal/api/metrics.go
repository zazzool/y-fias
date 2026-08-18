package api

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics — показатели §9.5 архитектуры.
//
// Отдельный реестр, а не глобальный: глобальный собирает метрики всего
// процесса, включая чужие библиотеки, и делает тесты зависимыми от порядка
// запуска.
type Metrics struct {
	registry *prometheus.Registry

	Duration  *prometheus.HistogramVec
	Errors    *prometheus.CounterVec
	LayerHits *prometheus.CounterVec
	Panics    prometheus.Counter

	SnapshotAge     prometheus.GaugeFunc
	SnapshotStreets prometheus.GaugeFunc
}

func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()

	m := &Metrics{
		registry: reg,
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "yfias_request_duration_seconds",
			Help: "Время обработки запроса по методам",
			// Границы подобраны под бюджет §9.3: p99 подсказки <= 5 мс.
			// Стандартные бакеты Prometheus начинаются с 5 мс и для нас
			// бесполезны — весь интересный диапазон схлопнулся бы в один.
			Buckets: []float64{
				0.00005, 0.0001, 0.00025, 0.0005, 0.001,
				0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.5,
			},
		}, []string{"method"}),

		Errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yfias_errors_total",
			Help: "Ошибки обработки запросов",
		}, []string{"method"}),

		LayerHits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "yfias_suggest_layer_hits_total",
			Help: "Попадания по слоям поиска: exact, prefix, substring, fuzzy",
		}, []string{"layer"}),

		Panics: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "yfias_panics_total",
			Help: "Паники в обработчиках",
		}),
	}

	reg.MustRegister(m.Duration, m.Errors, m.LayerHits, m.Panics)
	return m
}

// BindSnapshot подключает метрики, зависящие от состояния справочника.
//
// snapshot_age_seconds — сторож синхронизации: если справочник перестал
// обновляться, это видно раньше, чем по жалобам (§11.6 архитектуры).
func (m *Metrics) BindSnapshot(age func() time.Duration, streets func() int) {
	m.SnapshotAge = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "yfias_snapshot_age_seconds",
		Help: "Возраст загруженного справочника",
	}, func() float64 { return age().Seconds() })

	m.SnapshotStreets = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "yfias_snapshot_streets",
		Help: "Объектов в справочнике",
	}, func() float64 { return float64(streets()) })

	m.registry.MustRegister(m.SnapshotAge, m.SnapshotStreets)
}

func (m *Metrics) observe(method string, took time.Duration, err error) {
	m.Duration.WithLabelValues(method).Observe(took.Seconds())
	if err != nil {
		m.Errors.WithLabelValues(method).Inc()
	}
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// Registry — для тестов и для регистрации дополнительных метрик.
func (m *Metrics) Registry() *prometheus.Registry { return m.registry }
