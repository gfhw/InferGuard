package podwatch

import "time"

type Config struct {
	Endpoint       string
	Method         string
	Headers        map[string]string
	ResyncInterval time.Duration
	PrometheusAddr string
	Namespaces     []string
}
