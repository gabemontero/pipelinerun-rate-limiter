package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/gabemontero/pipelinerun-rate-limiter/pkg/reconciler/clusterresourcequota"
	"github.com/gabemontero/pipelinerun-rate-limiter/pkg/reconciler/k8sresourcequota"
	"github.com/gabemontero/pipelinerun-rate-limiter/pkg/reconciler/pendingpipelinerun"
	pipelinev1beta1 "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	apiextensionsclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	controllerLog = ctrl.Log.WithName("controller")
)

func NewManager(cfg *rest.Config, options ctrl.Options) (ctrl.Manager, error) {
	// do not check tekton in kcp

	// we have seen in e2e testing that this path can get invoked prior to the TaskRun CRD getting generated,
	// and controller-runtime does not retry on missing CRDs.
	// so we are going to wait on the CRDs existing before moving forward.
	apiextensionsClient := apiextensionsclient.NewForConfigOrDie(cfg)
	if err := wait.PollImmediate(time.Second*5, time.Minute*5, func() (done bool, err error) {
		_, err = apiextensionsClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), "taskruns.tekton.dev", metav1.GetOptions{})
		if err != nil {
			controllerLog.Info(fmt.Sprintf("get of taskrun CRD failed with: %s", err.Error()))
			return false, nil
		}
		controllerLog.Info("get of taskrun CRD returned successfully")
		return true, nil
	}); err != nil {
		controllerLog.Error(err, "timed out waiting for taskrun CRD to be created")
		return nil, err
	}

	options.Scheme = runtime.NewScheme()

	// pretty sure this is there by default but we will be explicit like build-service
	if err := k8sscheme.AddToScheme(options.Scheme); err != nil {
		return nil, err
	}

	if err := pipelinev1beta1.AddToScheme(options.Scheme); err != nil {
		return nil, err
	}

	var mgr ctrl.Manager
	var err error
	// this replaces the need for creating a non-caching client to access these various types
	options.ClientDisableCacheFor = []client.Object{}
	options.NewCache = cache.BuilderWithOptions(cache.Options{
		SelectorsByObject: cache.SelectorsByObject{
			&pipelinev1beta1.PipelineRun{}: {},
		}})

	mgr, err = ctrl.NewManager(cfg, options)

	if err != nil {
		return nil, err
	}

	if err = pendingpipelinerun.SetupPRReconcilerWithManager(mgr); err != nil {
		return nil, err
	}

	if err = clusterresourcequota.SetupNewReconciler(cfg); err != nil {
		return nil, err
	}

	if err = k8sresourcequota.SetupNewReconcilerWithManager(mgr); err != nil {
		return nil, err
	}

	return mgr, nil
}
