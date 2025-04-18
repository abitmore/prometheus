// Copyright 2015 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/prometheus/prometheus/discovery"
)

var _ discovery.DiscovererMetrics = (*kubernetesMetrics)(nil)

type kubernetesMetrics struct {
	eventCount    *prometheus.CounterVec
	failuresCount prometheus.Counter

	metricRegisterer discovery.MetricRegisterer
}

func newDiscovererMetrics(reg prometheus.Registerer, _ discovery.RefreshMetricsInstantiator) discovery.DiscovererMetrics {
	m := &kubernetesMetrics{
		eventCount: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: discovery.KubernetesMetricsNamespace,
				Name:      "events_total",
				Help:      "The number of Kubernetes events handled.",
			},
			[]string{"role", "event"},
		),
		failuresCount: prometheus.NewCounter(
			prometheus.CounterOpts{
				Namespace: discovery.KubernetesMetricsNamespace,
				Name:      "failures_total",
				Help:      "The number of failed WATCH/LIST requests.",
			},
		),
	}

	m.metricRegisterer = discovery.NewMetricRegisterer(reg, []prometheus.Collector{
		m.eventCount,
		m.failuresCount,
	})

	// Initialize metric vectors.
	for _, role := range []string{
		RoleEndpointSlice.String(),
		RoleEndpoint.String(),
		RoleNode.String(),
		RolePod.String(),
		RoleService.String(),
		RoleIngress.String(),
	} {
		for _, evt := range []string{
			MetricLabelRoleAdd,
			MetricLabelRoleDelete,
			MetricLabelRoleUpdate,
		} {
			m.eventCount.WithLabelValues(role, evt)
		}
	}

	m.failuresCount.Add(0)

	return m
}

// Register implements discovery.DiscovererMetrics.
func (m *kubernetesMetrics) Register() error {
	return m.metricRegisterer.RegisterMetrics()
}

// Unregister implements discovery.DiscovererMetrics.
func (m *kubernetesMetrics) Unregister() {
	m.metricRegisterer.UnregisterMetrics()
}
