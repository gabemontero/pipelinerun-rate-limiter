package pendingpipelinerun

import (
	"context"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	NamespaceLabel  string = "namespace"
	PendingMetric   string = "stonesoup_pending_pipelineruns_total"
	AbandonedMetric string = "stonesoup_abandoned_pipelineruns_total"
)

var (
	pendingDesc   *prometheus.Desc
	abandonedDesc *prometheus.Desc
	registered    = false
	sc            statsCollector
	regLock       = sync.Mutex{}

	logger = ctrl.Log.WithName("metrics")
)

func InitPrometheus(client client.Client) {
	regLock.Lock()
	defer regLock.Unlock()

	if registered {
		return
	}

	registered = true

	labels := []string{NamespaceLabel}
	pendingDesc = prometheus.NewDesc(PendingMetric,
		"Number of total PipelineRuns still in pending state",
		labels,
		nil)

	abandonedDesc = prometheus.NewDesc(AbandonedMetric,
		"Number of PipelineRuns abandoned because of scheduling constraints",
		labels,
		nil,
	)

	//TODO based on our openshift builds experience, we have talked about the notion of tracking adoption
	// of various stonesoup features (i.e. product mgmt is curious how much has feature X been used for the life of this cluster),
	// or even stonesoup "overall", based on PipelineRun Counts that are incremented
	// each time a PipelineRun comes through the reconciler for the first time (i.e. we label the PipelineRun as
	// part of bumping the metric so we only bump once), and then this metric is immune to PipelineRuns getting pruned.
	// i.e. newStat = prometheus.NewGaugeVec(...) and then newStat.Inc() if first time through
	// Conversely, for "devops" concerns, the collections of existing PipelineRuns is typically more of what is needed.

	sc = statsCollector{
		client: client,
	}

	metrics.Registry.MustRegister(&sc)
}

type statsCollector struct {
	client client.Client
}

func (sc *statsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- pendingDesc
	ch <- abandonedDesc
}

func (sc *statsCollector) Collect(ch chan<- prometheus.Metric) {
	prList := v1beta1.PipelineRunList{}
	opts := &client.ListOptions{Namespace: metav1.NamespaceAll}
	if err := sc.client.List(context.Background(), &prList, opts); err != nil {
		//TODO add log / event
		return
	}
	cts, err := PipelineRunStats(&prList)
	if err != nil {
		//TODO add log / event
		return
	}
	for ns, ct := range cts {
		ch <- prometheus.MustNewConstMetric(pendingDesc, prometheus.GaugeValue, float64(ct.PendingCount), ns)
		ch <- prometheus.MustNewConstMetric(abandonedDesc, prometheus.GaugeValue, float64(ct.AbandonedCount), ns)

	}
}
