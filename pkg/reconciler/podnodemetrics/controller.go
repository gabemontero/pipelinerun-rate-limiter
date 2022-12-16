package podnodemetrics

import (
	"k8s.io/client-go/rest"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

/*
Similar to openshift cluster resource quota controller runtime could not digest
the k8s metrics api; also, the metrics api does not provide informers

TODO - can we reverse engineer an informer implementation?
TODO - or should we just build short term watches for active pipelineruns?
*/

var (
	K8SMetricsClient metricsclientset.Interface
)

func Setup(cfg *rest.Config) {
	K8SMetricsClient = metricsclientset.NewForConfigOrDie(cfg)
}
