//go:build periodic
// +build periodic

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/gabemontero/pipelinerun-rate-limiter/pkg/reconciler/pendingpipelinerun"
	quotav1 "github.com/openshift/api/quota/v1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"knative.dev/pkg/apis"
)

func TestServiceRegistry(t *testing.T) {
	ta := setup(t, nil)

	// set up quota to enable throttling
	quota := &quotav1.ClusterResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("for-%s-deployments", ta.ns),
		},
		Spec: quotav1.ClusterResourceQuotaSpec{
			Quota: corev1.ResourceQuotaSpec{
				Hard: corev1.ResourceList{
					corev1.ResourceName("count/pods"): resource.MustParse("50"),
				},
			},
			Selector: quotav1.ClusterResourceQuotaSelector{
				AnnotationSelector: map[string]string{
					"openshift.io/requester": ta.ns,
				},
			},
		},
	}
	_, err := qutoaClient.QuotaV1().ClusterResourceQuotas().Create(context.TODO(), quota, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}

	//TODO start of more common logic to split into commonly used logic between
	// TestExampleRun and TestServiceRegistry.  Not doing that yet because of
	// active PRs by other team members, including the component based test which
	// will probably greatly rework this anyway.
	path, err := os.Getwd()
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	ta.Logf(fmt.Sprintf("current working dir: %s", path))

	httpClient := http.Client{}
	resp1, err1 := httpClient.Get("https://raw.githubusercontent.com/redhat-appstudio/jvm-build-service/main/deploy/base/maven-v0.2.yaml")
	if err1 != nil {
		debugAndFailTest(ta, err1.Error())
	}
	defer resp1.Body.Close()
	httpBytes, err2 := ioutil.ReadAll(resp1.Body)
	if err2 != nil {
		debugAndFailTest(ta, err2.Error())
	}
	ta.maven = &v1beta1.Task{}
	obj := decodeBytesToTektonObjbytes(httpBytes, ta.maven, ta)
	var ok bool
	ta.maven, ok = obj.(*v1beta1.Task)
	if !ok {
		debugAndFailTest(ta, fmt.Sprintf("file https://raw.githubusercontent.com/redhat-appstudio/jvm-build-service/main/deploy/base/maven-v0.2.yaml did not produce a task: %#v", obj))
	}
	// override images if need be
	analyserImage := os.Getenv("JVM_BUILD_SERVICE_REQPROCESSOR_IMAGE")
	if len(analyserImage) > 0 {
		ta.Logf(fmt.Sprintf("PR analyzer image: %s", analyserImage))
		for i, step := range ta.maven.Spec.Steps {
			if step.Name != "analyse-dependencies" {
				continue
			}
			ta.Logf(fmt.Sprintf("Updating analyse-dependencies step with image %s", analyserImage))
			ta.maven.Spec.Steps[i].Image = analyserImage
		}
	}
	ta.maven, err = tektonClient.TektonV1beta1().Tasks(ta.ns).Create(context.TODO(), ta.maven, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}

	resp2, err3 := httpClient.Get("https://raw.githubusercontent.com/redhat-appstudio/jvm-build-service/main/hack/examples/pipeline.yaml")
	defer resp2.Body.Close()
	httpBytes, err = ioutil.ReadAll(resp2.Body)
	if err3 != nil {
		debugAndFailTest(ta, err3.Error())
	}
	ta.pipeline = &v1beta1.Pipeline{}
	obj = decodeBytesToTektonObjbytes(httpBytes, ta.pipeline, ta)
	ta.pipeline, ok = obj.(*v1beta1.Pipeline)
	if !ok {
		debugAndFailTest(ta, fmt.Sprintf("file https://raw.githubusercontent.com/redhat-appstudio/jvm-build-service/main/hack/examples/pipeline.yaml did not produce a pipeline: %#v", obj))
	}
	ta.pipeline, err = tektonClient.TektonV1beta1().Pipelines(ta.ns).Create(context.TODO(), ta.pipeline, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}

	resp3, err4 := httpClient.Get("https://raw.githubusercontent.com/redhat-appstudio/jvm-build-service/main/hack/examples/run-service-registry.yaml")
	defer resp3.Body.Close()
	httpBytes, err = ioutil.ReadAll(resp3.Body)
	if err4 != nil {
		debugAndFailTest(ta, err4.Error())
	}
	ta.run = &v1beta1.PipelineRun{}
	obj = decodeBytesToTektonObjbytes(httpBytes, ta.run, ta)
	ta.run, ok = obj.(*v1beta1.PipelineRun)
	if !ok {
		debugAndFailTest(ta, fmt.Sprintf("file https://raw.githubusercontent.com/redhat-appstudio/jvm-build-service/main/hack/examples/run-service-registry.yaml did not produce a pipelinerun: %#v", obj))
	}
	ta.run, err = tektonClient.TektonV1beta1().PipelineRuns(ta.ns).Create(context.TODO(), ta.run, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}

	ta.t.Run("pipelinerun completes successfully", func(t *testing.T) {
		err = wait.PollImmediate(3*ta.interval, 3*ta.timeout, func() (done bool, err error) {
			pr, err := tektonClient.TektonV1beta1().PipelineRuns(ta.ns).Get(context.TODO(), ta.run.Name, metav1.GetOptions{})
			if err != nil {
				ta.Logf(fmt.Sprintf("get pr %s produced err: %s", ta.run.Name, err.Error()))
				return false, nil
			}
			if !pr.IsDone() {
				prBytes, err := json.MarshalIndent(pr, "", "  ")
				if err != nil {
					ta.Logf(fmt.Sprintf("problem marshalling in progress pipelinerun to bytes: %s", err.Error()))
					return false, nil
				}
				ta.Logf(fmt.Sprintf("in flight pipeline run: %s", string(prBytes)))
				return false, nil
			}
			if !pr.GetStatusCondition().GetCondition(apis.ConditionSucceeded).IsTrue() {
				prBytes, err := json.MarshalIndent(pr, "", "  ")
				if err != nil {
					ta.Logf(fmt.Sprintf("problem marshalling failed pipelinerun to bytes: %s", err.Error()))
					return false, nil
				}
				debugAndFailTest(ta, fmt.Sprintf("unsuccessful pipeline run: %s", string(prBytes)))
			}
			return true, nil
		})
		if err != nil {
			debugAndFailTest(ta, "timed out when waiting for the pipeline run to complete")
		}
	})

	validationOccurred := false
	ta.t.Run("maintain pipelinerun quota", func(t *testing.T) {
		wait.PollImmediate(1*time.Minute, 15*time.Minute, func() (done bool, err error) {
			ctx := context.TODO()
			prList, lerr := tektonClient.TektonV1beta1().PipelineRuns(ta.ns).List(ctx, metav1.ListOptions{})
			if lerr != nil {
				return false, nil
			}
			stats, serr := pendingpipelinerun.PipelineRunStats(prList)
			if serr != nil {
				ta.Logf(fmt.Sprintf("get stats produced err: %s", serr.Error()))
				return false, nil
			}
			stat, sok := stats[ta.ns]
			if !sok {
				ta.Logf(fmt.Sprintf("get stats found no stats for test namespace : %#v", stats))
			}
			if stat.ActiveCount >= 50 {
				debugAndFailTest(ta, fmt.Sprintf("50 or more active pipelineruns: %#v", stat))
			}
			validationOccurred = true
			ta.Logf(fmt.Sprintf("we have %d active pipeline runs", stat.ActiveCount))
			return false, nil
		})

	})

	if !validationOccurred {
		t.Fatalf("CHECK TEST LOGS FOR ERRORS")
	}
}
