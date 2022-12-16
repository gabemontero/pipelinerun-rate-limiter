package pendingpipelinerun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gabemontero/pipelinerun-rate-limiter/pkg/reconciler/podnodemetrics"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
	client              client.Client
	scheme              *runtime.Scheme
	eventRecorder       record.EventRecorder
	allocatableCPU      map[string]resource.Quantity
	allocatableMem      map[string]resource.Quantity
	totalAllocatableCPU resource.Quantity
	totalAllocatableMem resource.Quantity
}

func newPRReconciler(mgr ctrl.Manager) reconcile.Reconciler {
	rc := &ReconcilePendingPipelineRun{
		client:              mgr.GetClient(),
		scheme:              mgr.GetScheme(),
		eventRecorder:       mgr.GetEventRecorderFor("PendingPipelineRun"),
		allocatableMem:      map[string]resource.Quantity{},
		allocatableCPU:      map[string]resource.Quantity{},
		totalAllocatableCPU: resource.Quantity{},
		totalAllocatableMem: resource.Quantity{},
	}
	nodeList := &corev1.NodeList{}
	//TODO retry on error / poll
	ctx := context.Background()
	rc.client.List(ctx, nodeList)
	for _, node := range nodeList.Items {
		// could also check master vs. worker labels
		// if has schedule taint, skip
		if len(node.Spec.Taints) > 0 {
			continue
		}
		for key, val := range node.Status.Allocatable {
			switch key {
			case corev1.ResourceCPU:
				rc.allocatableCPU[node.Name] = val
				rc.totalAllocatableCPU.Add(val)
				log.Info(fmt.Sprintf("GGM node %s has %s cpu", node.Name, val.String()))
			case corev1.ResourceMemory:
				rc.allocatableMem[node.Name] = val
				rc.totalAllocatableMem.Add(val)
				log.Info(fmt.Sprintf("GGM node %s has %s mem", node.Name, val.String()))
			}
		}
	}
	log.Info(fmt.Sprintf("GGM cluster has %s cpu and %s mem", rc.totalAllocatableCPU.String(), rc.totalAllocatableMem.String()))
	return rc
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
		return reconcile.Result{}, client.IgnoreNotFound(r.unthrottleNextOnQueuePlusCleanup(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}))
	}

	if pr.IsPending() {
		return r.handlePending(ctx, pr)
	}

	if isAbandoned(pr) {
		return r.handleAbandoned(ctx, pr)
	}

	if pr.IsDone() || pr.IsCancelled() || pr.IsGracefullyCancelled() || pr.IsGracefullyStopped() {
		// terminal state, pop a pending item off the queue
		return reconcile.Result{}, client.IgnoreNotFound(r.unthrottleNextOnQueuePlusCleanup(ctx, types.NamespacedName{Namespace: pr.Namespace, Name: pr.Name}))
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

	/*
			using the controller runtime client for the metric list resulted in
			I1213 19:01:42.019450       1 reflector.go:255] Listing and watching *metrics.PodMetrics from sigs.k8s.io/controller-runtime/pkg/cache/internal/informers_map.go:277
			I1213 19:01:42.019635       1 request.go:914] Error in request: v1.ListOptions is not suitable for converting to "metrics.k8s.io/__internal" in scheme "pkg/controller/controller.go:49"
			W1213 19:01:42.019735       1 reflector.go:324] sigs.k8s.io/controller-runtime/pkg/cache/internal/informers_map.go:277: failed to list *metrics.PodMetrics: v1.ListOptions is not suitable for converting to "metrics.k8s.io/__internal" in scheme "pkg/controller/controller.go:49"
			E1213 19:01:42.019811       1 reflector.go:138] sigs.k8s.io/controller-runtime/pkg/cache/internal/informers_map.go:277: Failed to watch *metrics.PodMetrics: failed to list *metrics.PodMetrics: v1.ListOptions is not suitable for converting to "metrics.k8s.io/__internal" in scheme "pkg/controller/controller.go:49"


		    E1213 19:25:43.223373       1 reflector.go:138] sigs.k8s.io/controller-runtime/pkg/cache/internal/informers_map.go:277: Failed to watch *v1beta1.PodMetrics: the server does not allow this method on the requested resource (get pods.metrics.k8s.io)
	*/
	if podnodemetrics.K8SMetricsClient != nil {
		namespaceCPU := resource.Quantity{}
		namespaceMem := resource.Quantity{}
		clusterCPU := resource.Quantity{}
		clusterMem := resource.Quantity{}
		allNodesOverCPU := true
		allNodesOverMem := true
		podMetrics, err := podnodemetrics.K8SMetricsClient.MetricsV1beta1().PodMetricses(pr.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Info(fmt.Sprintf("GGM pod metrics for ns %s err: %s ", pr.Namespace, err.Error()))
		} else {
			for _, pm := range podMetrics.Items {
				for _, cm := range pm.Containers {
					for key, val := range cm.Usage {
						switch key {
						case corev1.ResourceCPU:
							namespaceCPU.Add(val)
						case corev1.ResourceMemory:
							namespaceMem.Add(val)
						}
					}
				}
			}
			if len(podMetrics.Items) > 20 {
				log.Info(fmt.Sprintf("GGM ns %s with %d pods has total cpu usage %s and total mem usage %s", pr.Namespace, len(podMetrics.Items), namespaceCPU.String(), namespaceMem.String()))
			}
		}
		//log.Info(fmt.Sprintf("GGM active PRs %d pod metrics %d for ns %s", cts.ActiveCount, len(podMetrics.Items), pr.Namespace))
		nodeMetrics, err := podnodemetrics.K8SMetricsClient.MetricsV1beta1().NodeMetricses().List(ctx, metav1.ListOptions{})
		if err != nil {
			log.Info(fmt.Sprintf("GGM node metrics err: %s", err.Error()))
		} else {
			for _, nm := range nodeMetrics.Items {
				_, ok := r.allocatableCPU[nm.Name]
				if !ok {
					continue
				}
				//GGM timestamp useless, same as time.Now, Window consistently a 1m0x Duration
				nodeCurrentCPU := resource.Quantity{}
				nodeCurrentMem := resource.Quantity{}

				for key, val := range nm.Usage {
					switch key {
					case corev1.ResourceCPU:
						nodeCurrentCPU = val
					case corev1.ResourceMemory:
						nodeCurrentMem = val
					}
				}
				clusterCPU.Add(nodeCurrentCPU)
				clusterMem.Add(nodeCurrentMem)
				nodeAvailMem := r.allocatableMem[nm.Name]
				nodeAvailCPU := r.allocatableCPU[nm.Name]
				allocMemFraction := float64(nodeCurrentMem.MilliValue()) / float64(nodeAvailMem.MilliValue())
				allocCpuFraction := float64(nodeCurrentCPU.MilliValue()) / float64(nodeAvailCPU.MilliValue())
				//log.Info(fmt.Sprintf("GGM node %s curr cpu %s curr mem %s curr cpu ratio %.2f curr mem ratio %.2f",
				//	nm.Name,
				//	nodeCurrentCPU.String(),
				//	nodeCurrentMem.String(),
				//	allocCpuFraction,
				//	allocMemFraction))
				if allocCpuFraction <= 0.70 {
					allNodesOverCPU = false
				}
				if allocMemFraction <= 0.70 {
					allNodesOverMem = false
				}
			}
		}
		//nsMemFraction := float64(namespaceMem.MilliValue()) / float64(r.totalAllocatableMem.MilliValue())
		//nsCPUFraction := float64(namespaceCPU.MilliValue()) / float64(r.totalAllocatableCPU.MilliValue())
		//log.Info(fmt.Sprintf("GGM ns %s uses %.2f of total cpu and %.2f of total mem", pr.Namespace, nsCPUFraction, nsMemFraction))

		if allNodesOverCPU {
			log.Info(fmt.Sprintf("GGMGGM thorttling %s because of cluster cpu", pr.Name))
			return r.throttle(ctx, pr)
		}

		if allNodesOverMem {
			log.Info(fmt.Sprintf("GGMGGM throttling %s because of cluster mem", pr.Name))
			return r.throttle(ctx, pr)
		}

		if hardPodCount > 0 {
			switch {
			case (cts.TotalCount - cts.DoneCount) < hardPodCount:
				// below hard pod count quota so remove PR pending
				break
			case cts.TotalCount == cts.PendingCount:
				// initial race condition possible if controller starts a bunch before we get events:
				break
			case (hardPodCount - quotaBuffer) <= cts.ActiveCount:
				return r.throttle(ctx, pr)
			}
		}

		//TODO GGM based on analysis of builds during periodic
		maxStepCPU := resource.MustParse("3000m")
		maxStepMem := resource.MustParse("3Gi")

		//TODO the myriad of ways to inject into a PipelineRun including bundles ?? for now, this works off of how jvm-build-servivce does it
		if pr.Spec.PipelineSpec != nil {
			spec := pr.Spec.PipelineSpec
			for _, task := range spec.Tasks {
				if task.TaskSpec != nil {
					for _, step := range task.TaskSpec.Steps {
						cpu := step.Resources.Limits.Cpu()
						mem := step.Resources.Limits.Memory()
						log.Info(fmt.Sprintf("GGM pr %s step %s has cpu %v", pr.Name, step.Name, cpu))
						if cpu != nil && cpu.MilliValue() > maxStepCPU.MilliValue() {
							maxStepCPU = *cpu
						}
						if mem != nil && mem.MilliValue() > maxStepMem.MilliValue() {
							maxStepMem = *mem
						}
					}
				}
			}
			enoughSpace := r.totalAllocatableCPU.MilliValue()-clusterCPU.MilliValue() >= maxStepCPU.MilliValue()
			log.Info(fmt.Sprintf("GGM cluster current cpu %d cluster avail cpu %d pr cpu need %d will fit %v", clusterCPU.MilliValue(), r.totalAllocatableCPU.MilliValue(), maxStepCPU.MilliValue(), enoughSpace))
			if !enoughSpace {
				log.Info(fmt.Sprintf("GGM throttling %s because of pr specific capacity", pr.Name))
				return r.throttle(ctx, pr)
			}
		}

		log.Info(fmt.Sprintf("GGMGGM unthrottling %s", pr.Name))
		// remove pending bit, make this one active
		pr.Spec.Status = ""
		return reconcile.Result{}, client.IgnoreNotFound(r.client.Update(ctx, pr))
	}

	if hardPodCount > 0 {
		switch {
		case (cts.TotalCount - cts.DoneCount) < hardPodCount:
			// below hard pod count quota so remove PR pending
			break
		case cts.TotalCount == cts.PendingCount:
			// initial race condition possible if controller starts a bunch before we get events:
			break
		case (hardPodCount - quotaBuffer) <= cts.ActiveCount:
			return r.throttle(ctx, pr)
		}
	}

	// remove pending bit, make this one active
	pr.Spec.Status = ""
	return reconcile.Result{}, client.IgnoreNotFound(r.client.Update(ctx, pr))
}

func (r *ReconcilePendingPipelineRun) throttle(ctx context.Context, pr *v1beta1.PipelineRun) (reconcile.Result, error) {
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

func (r *ReconcilePendingPipelineRun) unthrottleNextOnQueuePlusCleanup(ctx context.Context, nsName types.NamespacedName) error {
	var err error
	prList := v1beta1.PipelineRunList{}
	opts := &client.ListOptions{Namespace: nsName.Namespace}
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
