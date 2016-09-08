/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
)

type mockDeploymentStore struct {
	f func() ([]extensions.Deployment, error)
}

func (ds mockDeploymentStore) List() (deployments []extensions.Deployment, err error) {
	return ds.f()
}

func TestDeploymentCollector(t *testing.T) {
	// Fixed metadata on type and help text. We prepend this to every expected
	// output so we only have to modify a single place when doing adjustments.
	const metadata = `
		# HELP deployment_spec_paused Whether the deployment is paused and will not be processed by the deployment controller.
		# TYPE deployment_spec_paused gauge
		# HELP deployment_spec_replicas Number of desired pods for a deployment.
		# TYPE deployment_spec_replicas gauge
		# HELP deployment_status_replicas The number of replicas per deployment.
		# TYPE deployment_status_replicas gauge
		# HELP deployment_status_replicas_available The number of available replicas per deployment.
		# TYPE deployment_status_replicas_available gauge
		# HELP deployment_status_replicas_unavailable The number of unavailable replicas per deployment.
		# TYPE deployment_status_replicas_unavailable gauge
		# HELP deployment_status_replicas_updated The number of updated replicas per deployment.
		# TYPE deployment_status_replicas_updated gauge
		# HELP deployment_status_observed_generation The generation observed by the deployment controller.
		# TYPE deployment_status_observed_generation gauge
	`
	cases := []struct {
		depls []extensions.Deployment
		want  string
	}{
		{
			depls: []extensions.Deployment{
				{
					ObjectMeta: api.ObjectMeta{
						Name:      "depl1",
						Namespace: "ns1",
					},
					Status: extensions.DeploymentStatus{
						Replicas:            15,
						AvailableReplicas:   10,
						UnavailableReplicas: 5,
						UpdatedReplicas:     2,
						ObservedGeneration:  111,
					},
					Spec: extensions.DeploymentSpec{
						Replicas: 200,
					},
				}, {
					ObjectMeta: api.ObjectMeta{
						Name:      "depl2",
						Namespace: "ns2",
					},
					Status: extensions.DeploymentStatus{
						Replicas:            10,
						AvailableReplicas:   5,
						UnavailableReplicas: 0,
						UpdatedReplicas:     1,
						ObservedGeneration:  1111,
					},
					Spec: extensions.DeploymentSpec{
						Paused:   true,
						Replicas: 5,
					},
				},
			},
			want: metadata + `
				deployment_spec_paused{namespace="ns1",deployment="depl1"} 0
				deployment_spec_paused{namespace="ns2",deployment="depl2"} 1
				deployment_spec_replicas{namespace="ns1",deployment="depl1"} 200
				deployment_spec_replicas{namespace="ns2",deployment="depl2"} 5
				deployment_status_replicas{namespace="ns1",deployment="depl1"} 15
				deployment_status_replicas{namespace="ns2",deployment="depl2"} 10
				deployment_status_replicas_available{namespace="ns1",deployment="depl1"} 10
				deployment_status_replicas_available{namespace="ns2",deployment="depl2"} 5
				deployment_status_replicas_unavailable{namespace="ns1",deployment="depl1"} 5
				deployment_status_replicas_unavailable{namespace="ns2",deployment="depl2"} 0
				deployment_status_replicas_updated{namespace="ns1",deployment="depl1"} 2
				deployment_status_replicas_updated{namespace="ns2",deployment="depl2"} 1
				deployment_status_observed_generation{namespace="ns1",deployment="depl1"} 111
				deployment_status_observed_generation{namespace="ns2",deployment="depl2"} 1111
			`,
		},
	}
	for _, c := range cases {
		dc := &deploymentCollector{
			store: mockDeploymentStore{
				f: func() ([]extensions.Deployment, error) { return c.depls, nil },
			},
		}
		if err := gatherAndCompare(dc, c.want); err != nil {
			t.Errorf("unexpected collecting result:\n%s", err)
		}
	}
}

// gatherAndCompare retrieves all metrics exposed by a collector and compares it
// to an expected output in the Prometheus text exposition format.
func gatherAndCompare(c prometheus.Collector, expected string) error {
	expected = removeUnusedWhitespace(expected)

	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		return fmt.Errorf("registering collector failed: %s", err)
	}
	metrics, err := reg.Gather()
	if err != nil {
		return fmt.Errorf("gathering metrics failed: %s", err)
	}
	var tp expfmt.TextParser
	expectedMetrics, err := tp.TextToMetricFamilies(bytes.NewReader([]byte(expected)))
	if err != nil {
		return fmt.Errorf("parsing expected metrics failed: %s", err)
	}

	if !reflect.DeepEqual(metrics, normalizeMetricFamilies(expectedMetrics)) {
		// Encode the gathered output to the readbale text format for comparison.
		var buf1 bytes.Buffer
		enc := expfmt.NewEncoder(&buf1, expfmt.FmtText)
		for _, mf := range metrics {
			if err := enc.Encode(mf); err != nil {
				return fmt.Errorf("encoding result failed: %s", err)
			}
		}
		// Encode normalized expected metrics again to generate them in the same ordering
		// the registry does to spot differences more easily.
		var buf2 bytes.Buffer
		enc = expfmt.NewEncoder(&buf2, expfmt.FmtText)
		for _, mf := range normalizeMetricFamilies(expectedMetrics) {
			if err := enc.Encode(mf); err != nil {
				return fmt.Errorf("encoding result failed: %s", err)
			}
		}

		return fmt.Errorf(`
metric output does not match expectation; want:

%s

got:

%s       
`, buf2.String(), buf1.String())
	}
	return nil
}

func removeUnusedWhitespace(s string) string {
	var (
		trimmedLine  string
		trimmedLines []string
		lines        []string = strings.Split(s, "\n")
	)

	for _, l := range lines {
		trimmedLine = strings.TrimSpace(l)

		if len(trimmedLine) > 0 {
			trimmedLines = append(trimmedLines, trimmedLine)
		}
	}

	// The Prometheus metrics representation parser expects an empty line at the
	// end otherwise fails with an unexpected EOF error.
	return strings.Join(trimmedLines, "\n") + "\n"
}

// The below sorting code is copied form the Prometheus client library modulo the added
// label pair sorting.
// https://github.com/prometheus/client_golang/blob/ea6e1db4cb8127eeb0b6954f7320363e5451820f/prometheus/registry.go#L642-L684

// metricSorter is a sortable slice of *dto.Metric.
type metricSorter []*dto.Metric

func (s metricSorter) Len() int {
	return len(s)
}

func (s metricSorter) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s metricSorter) Less(i, j int) bool {
	sort.Sort(prometheus.LabelPairSorter(s[i].Label))
	sort.Sort(prometheus.LabelPairSorter(s[j].Label))

	if len(s[i].Label) != len(s[j].Label) {
		return len(s[i].Label) < len(s[j].Label)
	}

	for n, lp := range s[i].Label {
		vi := lp.GetValue()
		vj := s[j].Label[n].GetValue()
		if vi != vj {
			return vi < vj
		}
	}

	if s[i].TimestampMs == nil {
		return false
	}
	if s[j].TimestampMs == nil {
		return true
	}
	return s[i].GetTimestampMs() < s[j].GetTimestampMs()
}

// normalizeMetricFamilies returns a MetricFamily slice whith empty
// MetricFamilies pruned and the remaining MetricFamilies sorted by name within
// the slice, with the contained Metrics sorted within each MetricFamily.
func normalizeMetricFamilies(metricFamiliesByName map[string]*dto.MetricFamily) []*dto.MetricFamily {
	for _, mf := range metricFamiliesByName {
		sort.Sort(metricSorter(mf.Metric))
	}
	names := make([]string, 0, len(metricFamiliesByName))
	for name, mf := range metricFamiliesByName {
		if len(mf.Metric) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := make([]*dto.MetricFamily, 0, len(names))
	for _, name := range names {
		result = append(result, metricFamiliesByName[name])
	}
	return result
}
