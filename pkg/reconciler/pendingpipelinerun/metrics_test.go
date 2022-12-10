package pendingpipelinerun

import (
	"context"
	"testing"
	"time"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	. "github.com/onsi/gomega"
)

//NOTE:  our default mode of running unit tests in parallel, coupled with prometheus treating
// counters as global, makes vetting precise metric Counts across multiple tests problematic;
// so we have cover our various "scenarios" under one test method

func gatherMetrics(g *WithT) (abandonedValue float64, pendingValue float64) {
	metrics, err := crmetrics.Registry.Gather()
	g.Expect(err).NotTo(HaveOccurred())
	for _, metricFamily := range metrics {
		switch metricFamily.GetName() {
		case AbandonedMetric:
			for _, m := range metricFamily.GetMetric() {
				abandonedValue = abandonedValue + m.GetGauge().GetValue()
			}
		case PendingMetric:
			for _, m := range metricFamily.GetMetric() {
				pendingValue = pendingValue + m.GetGauge().GetValue()
			}
		}
	}
	return abandonedValue, pendingValue
}

func createPRs(g *WithT, t *testing.T, ctx context.Context, client runtimeclient.Client, reconciler *ReconcilePendingPipelineRun, createTimes []metav1.Time) []v1beta1.PipelineRun {
	prs := []v1beta1.PipelineRun{
		{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "test1"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "test2"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "test3"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "test4"}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: metav1.NamespaceDefault, Name: "test5"}},
	}
	if createTimes != nil {
		if len(createTimes) != 5 {
			t.Fatal("need to pass in an array for all 5 items")
		}
		for i := range prs {
			prs[i].CreationTimestamp = createTimes[i]
		}
	}
	for _, pr := range prs {
		err := CreatePipelineRunWithMutations(ctx, client, &pr)
		g.Expect(err).NotTo(HaveOccurred())

		g.Expect(reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}}))
	}
	return prs
}

func TestMetrics(t *testing.T) {
	g := NewGomegaWithT(t)
	quota := &corev1.ResourceQuota{
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourcePods: resource.MustParse("5"),
			},
		},
	}
	quota.Namespace = metav1.NamespaceDefault
	quota.Name = "foo"
	client, reconciler := SetupClientAndPRReconciler(false, quota)
	InitPrometheus(client)
	ctx := context.TODO()
	//NOTE no creation timestamp set (i.e. IsZero) will not register as timed out
	prs := createPRs(g, t, ctx, client, reconciler, nil)

	// TEST 1 - on initial create of 5 with pod quota of 5, the last PR will be left in pending, and the counter bumped
	abandonedValue, pendingValue := gatherMetrics(g)
	g.Expect(abandonedValue).To(Equal(float64(0)))
	g.Expect(pendingValue).To(Equal(float64(1)))

	for _, pr := range prs {
		g.Expect(reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}}))
	}

	// TEST 2 - reconcile the pending item again, but with label there, it will not bump the pending count again
	abandonedValue, pendingValue = gatherMetrics(g)
	g.Expect(abandonedValue).To(Equal(float64(0)))
	g.Expect(pendingValue).To(Equal(float64(1)))

	// any PR from test1 to test4 should be active, pick one and mark complete, then reconcile on it
	key := runtimeclient.ObjectKey{Namespace: prs[2].Namespace, Name: prs[2].Name}
	pr := v1beta1.PipelineRun{}
	err := client.Get(ctx, key, &pr)
	g.Expect(err).NotTo(HaveOccurred())
	pr.Status.MarkSucceeded(string(v1beta1.PipelineRunReasonSuccessful), "")
	err = client.Update(ctx, &pr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}}))

	// TEST 3 - after our completed PR has reconciled, the pending PR should have been activated, and pending decremented
	abandonedValue, pendingValue = gatherMetrics(g)
	g.Expect(abandonedValue).To(Equal(float64(0)))
	g.Expect(pendingValue).To(Equal(float64(0)))

	// TEST 4 - reconcile all the now non-pending pr's and make sure the Counts are ok
	for _, pr := range prs {
		g.Expect(reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}}))
	}
	abandonedValue, pendingValue = gatherMetrics(g)
	g.Expect(abandonedValue).To(Equal(float64(0)))
	g.Expect(pendingValue).To(Equal(float64(0)))

	// clear out all PRs, so we can create some with a timeout that will force an abandon
	for _, pr := range prs {
		client.Delete(ctx, &pr)
	}

	// TEST 5 - create a timed out PR so the abandon count is bumped
	createTimes := []metav1.Time{
		metav1.NewTime(time.Now()),
		metav1.NewTime(time.Now()),
		metav1.NewTime(time.Now()),
		metav1.NewTime(time.Now()),
		metav1.NewTime(time.Date(2021, 01, 01, 0, 0, 0, 0, time.UTC)),
	}
	prs = createPRs(g, t, ctx, client, reconciler, createTimes)
	// reconcile timed out item after the last reconcile cancelled and set the abandon annotation; should bump counter
	g.Expect(reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: prs[4].Namespace, Name: prs[4].Name}}))
	abandonedValue, pendingValue = gatherMetrics(g)
	g.Expect(abandonedValue).To(Equal(float64(1)))
	g.Expect(pendingValue).To(Equal(float64(0)))

}
