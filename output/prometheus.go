// Package output implements data outputter interfaces.
package output

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/northeye/chissoku/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus outputter Prometheus struct
type Prometheus struct {
	Port int `long:"port" help:"port number for prometheus. default: '9090'" default:"9090"`

	current *types.Data

	cancel func()
}

var (
	co2 = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "co2",
			Help: "CO2 concentration",
		},
		[]string{"tag"},
	)
	humidity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "humidity",
			Help: "Humidity",
		},
		[]string{"tag"},
	)
	temperature = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "temperature",
			Help: "Temperature",
		},
		[]string{"tag"},
	)
)

// Close Prometheus implementation
func (*Prometheus) Close() {
}

// Name Prometheus implementation
func (b *Prometheus) Name() string {
	return strings.ToLower(reflect.TypeOf(b).Elem().Name())
}

// Output Prometheus implementation
func (b *Prometheus) Output(d *types.Data) {
	b.current = d

	co2.WithLabelValues("CO2").Set(float64(d.CO2))
	humidity.WithLabelValues("Humidity").Set(d.Humidity)
	temperature.WithLabelValues("Temperature").Set(d.Temperature)
}

// Initialize initialize outputter
func (b *Prometheus) Initialize(_ context.Context) (_ error) {
	prometheus.MustRegister(co2)
	prometheus.MustRegister(humidity)
	prometheus.MustRegister(temperature)

	go func() {
		http.Handle("/metrics", promhttp.Handler())
		http.ListenAndServe(fmt.Sprintf(":%d", b.Port), nil)
	}()

	return
}
