package metrics

import "github.com/prometheus/client_golang/prometheus"

var installationReconciliations = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "previa_installation_reconciliation_total", Help: "Authoritative installation reconciliation outcomes; changes indicate a missed webhook."}, []string{"outcome"})

func init() { Registry.MustRegister(installationReconciliations) }
func InstallationReconciliation(outcome string, count int) {
	installationReconciliations.WithLabelValues(bounded(outcome, "deleted", "suspended", "repository_removed", "unchanged", "error", "circuit_open")).Add(float64(max(count, 0)))
}
