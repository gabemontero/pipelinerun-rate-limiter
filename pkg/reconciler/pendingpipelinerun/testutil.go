package pendingpipelinerun

import (
	"github.com/gabemontero/pipelinerun-rate-limiter/pkg/reconciler/clusterresourcequota"
	quotav1 "github.com/openshift/api/quota/v1"
	fakequotaclientset "github.com/openshift/client-go/quota/clientset/versioned/fake"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/api/node/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func SetupClientAndPRReconciler(useOpenshift bool, objs ...runtimeclient.Object) (runtimeclient.Client, *ReconcilePendingPipelineRun) {
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)
	_ = v1beta1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = quotav1.AddToScheme(scheme)
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	if useOpenshift {
		clusterresourcequota.QuotaClient = fakequotaclientset.NewSimpleClientset()
	}
	return client, &ReconcilePendingPipelineRun{client: client, scheme: scheme, eventRecorder: &record.FakeRecorder{}}
}
