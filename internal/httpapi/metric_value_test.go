package httpapi

import (
	"strconv"
	"strings"

	"github.com/ElcanoTek/fleet/internal/metrics"
)

// metricValue returns the current value of one exact Prometheus series (name
// plus labels, as metrics.Render prints it), or 0 when it has not been
// recorded yet. The registry is process-wide, so tests compare a before/after
// delta rather than an absolute count.
func metricValue(series string) float64 {
	for _, line := range strings.Split(metrics.Render(), "\n") {
		if rest, ok := strings.CutPrefix(line, series+" "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
			if err == nil {
				return v
			}
		}
	}
	return 0
}
