package pendingpipelinerun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"knative.dev/pkg/apis"
)

const (
	//TODO add fields for both constants to jbsconfig, systemconfig, or both
	abandonAfter = 3 * time.Hour
	requeueAfter = 1 * time.Minute
	//TODO subtracting 1 for the jvm-bld-svc artifact cache, then another 3 for safety buffer, but tuning this default after broader testing likely
	quotaBuffer    = 4
	contextTimeout = 300 * time.Second

	//TODO move to API folder, along with config obj changes
	AbandonedAnnotation = "openshift-pipelines/cancelled-to-abandon"
	AbandonedReason     = "Abandoned"
	AbandonedMessage    = "PipelineRun %s:%s abandoned because of Pod Quota pressure"
)

var (
	log = ctrl.Log.WithName("pendingpipelinerunreconciler")
)

type ReconcilePendingPipelineRun struct {
	client        client.Client
	scheme        *runtime.Scheme
	eventRecorder record.EventRecorder
}

func newPRReconciler(mgr ctrl.Manager) reconcile.Reconciler {
	return &ReconcilePendingPipelineRun{
		client:        mgr.GetClient(),
		scheme:        mgr.GetScheme(),
		eventRecorder: mgr.GetEventRecorderFor("PendingPipelineRun"),
	}
}

func (r *ReconcilePendingPipelineRun) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	// Set the ctx to be Background, as the top-level context for incoming requests.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, contextTimeout)
	defer cancel()

	pr := &v1beta1.PipelineRun{}
	prerr := r.client.Get(ctx, request.NamespacedName, pr)
	if prerr != nil {
		if !errors.IsNotFound(prerr) {
			return ctrl.Result{}, prerr
		}
	}

	if prerr != nil {
		msg := fmt.Sprintf("Reconcile key %s received not found errors for both pipelineruns and pendingpipelinerun (probably deleted)\"", request.NamespacedName.String())
		log.Info(msg)
		return reconcile.Result{}, client.IgnoreNotFound(r.unthrottleNextOnQueuePlusCleanup(ctx, pr.Namespace))
	}

	if pr.IsPending() {
		return r.handlePending(ctx, pr)
	}

	if isAbandoned(pr) {
		return r.handleAbandoned(ctx, pr)
	}

	if pr.IsDone() || pr.IsCancelled() || pr.IsGracefullyCancelled() || pr.IsGracefullyStopped() {
		// terminal state, pop a pending item off the queue
		return reconcile.Result{}, client.IgnoreNotFound(r.unthrottleNextOnQueuePlusCleanup(ctx, pr.Namespace))
	}

	return reconcile.Result{}, nil

}

func isAbandoned(pr *v1beta1.PipelineRun) bool {
	aval, aok := pr.Annotations[AbandonedAnnotation]
	condition := pr.Status.GetCondition(apis.ConditionSucceeded)
	needToAddAbandonReasonMessage := condition == nil || condition.Reason != AbandonedReason
	return pr.IsCancelled() && aok && strings.TrimSpace(aval) == "true" && needToAddAbandonReasonMessage
}

func isAbandonedPlusFailed(pr *v1beta1.PipelineRun) bool {
	aval, aok := pr.Annotations[AbandonedAnnotation]
	condition := pr.Status.GetCondition(apis.ConditionSucceeded)
	hasReason := condition != nil && condition.Reason == AbandonedReason
	return pr.IsCancelled() && aok && strings.TrimSpace(aval) == "true" && hasReason

}

func (r *ReconcilePendingPipelineRun) handleAbandoned(ctx context.Context, pr *v1beta1.PipelineRun) (reconcile.Result, error) {
	// assumes cancelled with our annotation per r.isAbandoned(), so let's bump abandoned metric and mark failed
	pr.Status.MarkFailed(AbandonedReason, AbandonedMessage, pr.Namespace, pr.Name)
	return reconcile.Result{}, client.IgnoreNotFound(r.client.Status().Update(ctx, pr))

}

func (r *ReconcilePendingPipelineRun) handlePending(ctx context.Context, pr *v1beta1.PipelineRun) (reconcile.Result, error) {
	// have a pending PR
	hardPodCount, pcerr := getHardPodCount(ctx, r.client, pr.Namespace)
	if pcerr != nil {
		return reconcile.Result{}, pcerr
	}

	if hardPodCount > 0 {
		prList := v1beta1.PipelineRunList{}
		opts := &client.ListOptions{Namespace: pr.Namespace}
		if err := r.client.List(ctx, &prList, opts); err != nil {
			return reconcile.Result{}, err
		}
		ret, lerr := PipelineRunStats(&prList)
		if lerr != nil {
			return reconcile.Result{}, lerr
		}
		cts, _ := ret[pr.Namespace]

		switch {
		case (cts.TotalCount - cts.DoneCount) < hardPodCount:
			// below hard pod count quota so remove PR pending
			break
		case cts.TotalCount == cts.PendingCount:
			// initial race condition possible if controller starts a bunch before we get events:
			break
		case (hardPodCount - quotaBuffer) <= cts.ActiveCount:
			// see if pending item still has to wait
			if !r.timedOut(pr) {
				return reconcile.Result{RequeueAfter: requeueAfter}, nil
			}
			// pending item has waited too long, cancel with an event to explain
			pr.Spec.Status = v1beta1.PipelineRunSpecStatusCancelled
			if pr.Annotations == nil {
				pr.Annotations = map[string]string{}
			}
			pr.Annotations[AbandonedAnnotation] = "true"
			r.eventRecorder.Eventf(pr, corev1.EventTypeWarning, "AbandonedPipelineRun", "after throttling, now past throttling timeout and have to abandon %s:%s", pr.Namespace, pr.Name)
			return reconcile.Result{}, client.IgnoreNotFound(r.client.Update(ctx, pr))
		}
	}

	// remove pending bit, make this one active
	pr.Spec.Status = ""
	return reconcile.Result{}, client.IgnoreNotFound(r.client.Update(ctx, pr))
}

func (r *ReconcilePendingPipelineRun) unthrottleNextOnQueuePlusCleanup(ctx context.Context, namespace string) error {
	var err error
	prList := v1beta1.PipelineRunList{}
	opts := &client.ListOptions{Namespace: namespace}
	if err = r.client.List(ctx, &prList, opts); err != nil {
		return err
	}
	for i, pr := range prList.Items {
		if pr.IsPending() {
			_, err = r.handlePending(ctx, &prList.Items[i])
			return err
		}
	}
	return nil
}

func (r *ReconcilePendingPipelineRun) timedOut(pr *v1beta1.PipelineRun) bool {
	// if not set yet, say not timed out
	if pr.ObjectMeta.CreationTimestamp.IsZero() {
		return false
	}
	timeout := pr.ObjectMeta.CreationTimestamp.Add(abandonAfter)
	return timeout.Before(time.Now())
}

type Counts struct {
	ActiveCount    int
	PendingCount   int
	AbandonedCount int
	DoneCount      int
	TotalCount     int
}

func PipelineRunStats(prList *v1beta1.PipelineRunList) (map[string]Counts, error) {
	var err error
	ret := map[string]Counts{}
	for _, p := range prList.Items {
		ct, ok := ret[p.Namespace]
		if !ok {
			ct = Counts{}
		}
		ct.TotalCount++
		switch {
		case p.IsPending():
			ct.PendingCount++
		case isAbandonedPlusFailed(&p):
			ct.AbandonedCount++
		case p.IsDone() || p.IsCancelled() || p.IsGracefullyCancelled() || p.IsGracefullyStopped():
			ct.DoneCount++
		default:
			ct.ActiveCount++
		}
		ret[p.Namespace] = ct

	}
	return ret, err
}
