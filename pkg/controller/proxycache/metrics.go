// SPDX-FileCopyrightText: Copyright (c) 2025-2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxycache

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// proxyCacheHealthy reports, per ProxyCache, whether Harbor considers its
// registry endpoint healthy (1) or not (0). It is the alerting signal for
// proxy-cache outages such as a rejected upstream credential, which Harbor's
// own exporter does not expose. Scraped via the controller-runtime metrics
// endpoint; alert on proxycache_healthy < 1.
var proxyCacheHealthy = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "proxycache_healthy",
		Help: "Whether the Harbor proxy-cache registry endpoint is healthy (1) or not (0).",
	},
	[]string{"proxycache", "registry_type"},
)

func init() {
	metrics.Registry.MustRegister(proxyCacheHealthy)
}

// setHealthMetric records the health gauge for a ProxyCache. healthy is true
// only when Harbor reports the endpoint as "healthy".
func setHealthMetric(name, registryType string, healthy bool) {
	v := 0.0
	if healthy {
		v = 1.0
	}
	proxyCacheHealthy.WithLabelValues(name, registryType).Set(v)
}

// deleteHealthMetric removes a ProxyCache's gauge series so a deleted resource
// does not linger as a stale timeseries until the operator restarts.
func deleteHealthMetric(name, registryType string) {
	proxyCacheHealthy.DeleteLabelValues(name, registryType)
}
