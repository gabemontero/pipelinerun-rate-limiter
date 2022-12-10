package e2e

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	projectv1 "github.com/openshift/api/project/v1"
	"github.com/redhat-appstudio/jvm-build-service/pkg/apis/jvmbuildservice/v1alpha1"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilrand "k8s.io/apimachinery/pkg/util/rand"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/rest"
)

func generateName(base string) string {
	if len(base) > maxGeneratedNameLength {
		base = base[:maxGeneratedNameLength]
	}
	return fmt.Sprintf("%s%s", base, utilrand.String(randomLength))
}

func dumpBadEvents(ta *testArgs) {
	eventClient := kubeClient.EventsV1().Events(ta.ns)
	eventList, err := eventClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		ta.Logf(fmt.Sprintf("error listing events: %s", err.Error()))
		return
	}
	ta.Logf(fmt.Sprintf("dumpBadEvents have %d items in total list", len(eventList.Items)))
	for _, event := range eventList.Items {
		if event.Type == corev1.EventTypeNormal {
			continue
		}
		ta.Logf(fmt.Sprintf("non-normal event reason %s about obj %s:%s message %s", event.Reason, event.Regarding.Kind, event.Regarding.Name, event.Note))
	}
}

func dumpNodes(ta *testArgs) {
	nodeClient := kubeClient.CoreV1().Nodes()
	nodeList, err := nodeClient.List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		ta.Logf(fmt.Sprintf("error listin nodes: %s", err.Error()))
		return
	}
	ta.Logf(fmt.Sprintf("dumpNodes found %d nodes in list, but only logging worker nodes", len(nodeList.Items)))
	for _, node := range nodeList.Items {
		_, ok := node.Labels["node-role.kubernetes.io/master"]
		if ok {
			continue
		}
		if node.Status.Allocatable.Cpu() == nil {
			ta.Logf(fmt.Sprintf("Node %s does not have allocatable cpu", node.Name))
			continue
		}
		if node.Status.Allocatable.Memory() == nil {
			ta.Logf(fmt.Sprintf("Node %s does not have allocatable mem", node.Name))
			continue
		}
		if node.Status.Capacity.Cpu() == nil {
			ta.Logf(fmt.Sprintf("Node %s does not have capacity cpu", node.Name))
			continue
		}
		if node.Status.Capacity.Memory() == nil {
			ta.Logf(fmt.Sprintf("Node %s does not have capacity mem", node.Name))
			continue
		}
		alloccpu := node.Status.Allocatable.Cpu()
		allocmem := node.Status.Allocatable.Memory()
		capaccpu := node.Status.Capacity.Cpu()
		capacmem := node.Status.Capacity.Memory()
		ta.Logf(fmt.Sprintf("Node %s allocatable CPU %s allocatable mem %s capacity CPU %s capacitymem %s",
			node.Name,
			alloccpu.String(),
			allocmem.String(),
			capaccpu.String(),
			capacmem.String()))
	}
}

func debugAndFailTest(ta *testArgs, failMsg string) {
	dumpBadEvents(ta)
	ta.t.Fatalf(failMsg)

}

func setup(t *testing.T, ta *testArgs) *testArgs {
	if ta == nil {
		ta = &testArgs{
			t:        t,
			timeout:  time.Minute * 10,
			interval: time.Second * 15,
		}
	}
	setupClients(ta.t)

	if len(ta.ns) == 0 {
		ta.ns = generateName(testNamespace)
		_, err := projectClient.ProjectV1().ProjectRequests().Create(context.Background(), &projectv1.ProjectRequest{
			ObjectMeta: metav1.ObjectMeta{Name: ta.ns},
		}, metav1.CreateOptions{})

		if err != nil {
			debugAndFailTest(ta, fmt.Sprintf("%#v", err))
		}
	}

	dumpNodes(ta)

	var err error
	err = wait.PollImmediate(1*time.Second, 1*time.Minute, func() (done bool, err error) {
		_, err = kubeClient.CoreV1().ServiceAccounts(ta.ns).Get(context.TODO(), "pipeline", metav1.GetOptions{})
		if err != nil {
			ta.Logf(fmt.Sprintf("get of pipeline SA err: %s", err.Error()))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		debugAndFailTest(ta, "pipeline SA not created in timely fashion")
	}

	// have seen delays in CRD presence along with missing pipeline SA
	err = wait.PollImmediate(1*time.Second, 1*time.Minute, func() (done bool, err error) {
		_, err = apiextensionClient.ApiextensionsV1().CustomResourceDefinitions().Get(context.TODO(), "tasks.tekton.dev", metav1.GetOptions{})
		if err != nil {
			ta.Logf(fmt.Sprintf("get of task CRD: %s", err.Error()))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		debugAndFailTest(ta, "task CRD not present in timely fashion")
	}

	ta.gitClone = &v1beta1.Task{}
	obj := streamRemoteYamlToTektonObj(gitCloneTaskUrl, ta.gitClone, ta)
	var ok bool
	ta.gitClone, ok = obj.(*v1beta1.Task)
	if !ok {
		debugAndFailTest(ta, fmt.Sprintf("%s did not produce a task: %#v", gitCloneTaskUrl, obj))
	}
	ta.gitClone, err = tektonClient.TektonV1beta1().Tasks(ta.ns).Create(context.TODO(), ta.gitClone, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	owner := os.Getenv("QUAY_E2E_ORGANIZATION")
	if owner == "" {
		owner = "redhat-appstudio-qe"
	}
	jbsConfig := v1alpha1.JBSConfig{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ta.ns,
			Name:      v1alpha1.JBSConfigName,
		},
		Spec: v1alpha1.JBSConfigSpec{
			EnableRebuilds: true,
			MavenBaseLocations: map[string]string{
				"maven-repository-300-jboss":                      "https://repository.jboss.org/nexus/content/groups/public/",
				"maven-repository-301-gradleplugins":              "https://plugins.gradle.org/m2",
				"maven-repository-302-confluent":                  "https://packages.confluent.io/maven",
				"maven-repository-303-gradle":                     "https://repo.gradle.org/artifactory/libs-releases",
				"maven-repository-304-eclipselink":                "https://download.eclipse.org/rt/eclipselink/maven.repo",
				"maven-repository-305-redhat":                     "https://maven.repository.redhat.com/ga",
				"maven-repository-306-jitpack":                    "https://jitpack.io",
				"maven-repository-307-jsweet":                     "https://repository.jsweet.org/artifactory/libs-release-local",
				"maven-repository-308-jenkins":                    "https://repo.jenkins-ci.org/public/",
				"maven-repository-309-spring-plugins":             "https://repo.springsource.org/plugins-release",
				"maven-repository-310-dokkadev":                   "https://maven.pkg.jetbrains.space/kotlin/p/dokka/dev",
				"maven-repository-311-ajoberstar":                 "https://ajoberstar.org/bintray-backup",
				"maven-repository-312-googleandroid":              "https://dl.google.com/dl/android/maven2/",
				"maven-repository-313-kotlinnative14linux":        "https://download.jetbrains.com/kotlin/native/builds/releases/1.4/linux",
				"maven-repository-314-jcs":                        "https://packages.jetbrains.team/maven/p/jcs/maven",
				"maven-repository-315-kotlin-bootstrap":           "https://maven.pkg.jetbrains.space/kotlin/p/kotlin/bootstrap/",
				"maven-repository-315-kotlin-kotlin-dependencies": "https://maven.pkg.jetbrains.space/kotlin/p/kotlin/kotlin-dependencies"},

			CacheSettings: v1alpha1.CacheSettings{},
			ImageRegistry: v1alpha1.ImageRegistry{
				Host:       "quay.io",
				Owner:      owner,
				Repository: "test-images",
				PrependTag: strconv.FormatInt(time.Now().UnixMilli(), 10),
			},
			RelocationPatterns: []v1alpha1.RelocationPatternElement{
				{
					RelocationPattern: v1alpha1.RelocationPattern{
						BuildPolicy: "default",
						Patterns: []v1alpha1.PatternElement{
							{
								Pattern: v1alpha1.Pattern{
									From: "(io.github.stuartwdouglas.hacbs-test.simple):(simple-jdk17):(99-does-not-exist)",
									To:   "io.github.stuartwdouglas.hacbs-test.simple:simple-jdk17:0.1.2",
								},
							},
							{
								Pattern: v1alpha1.Pattern{
									From: "org.graalvm.sdk:graal-sdk:21.3.2",
									To:   "org.graalvm.sdk:graal-sdk:21.3.2.0-1-redhat-00001",
								},
							},
						},
					},
				},
			},
		},
		Status: v1alpha1.JBSConfigStatus{},
	}
	_, err = jvmClient.JvmbuildserviceV1alpha1().JBSConfigs(ta.ns).Create(context.TODO(), &jbsConfig, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	decoded, err := base64.StdEncoding.DecodeString(os.Getenv("QUAY_TOKEN"))
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	secret := corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "jvm-build-image-secrets", Namespace: ta.ns},
		Data: map[string][]byte{".dockerconfigjson": decoded}}
	_, err = kubeClient.CoreV1().Secrets(ta.ns).Create(context.TODO(), &secret, metav1.CreateOptions{})
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	err = wait.PollImmediate(1*time.Second, 1*time.Minute, func() (done bool, err error) {
		_, err = kubeClient.AppsV1().Deployments(ta.ns).Get(context.TODO(), v1alpha1.CacheDeploymentName, metav1.GetOptions{})
		if err != nil {
			ta.Logf(fmt.Sprintf("get of cache: %s", err.Error()))
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		debugAndFailTest(ta, "cache not present in timely fashion")
	}
	return ta
}

//func projectCleanup(ta *testArgs) {
//	projectClient.ProjectV1().Projects().Delete(context.Background(), ta.ns, metav1.DeleteOptions{})
//}

func decodeBytesToTektonObjbytes(bytes []byte, obj runtime.Object, ta *testArgs) runtime.Object {
	decodingScheme := runtime.NewScheme()
	utilruntime.Must(v1beta1.AddToScheme(decodingScheme))
	decoderCodecFactory := serializer.NewCodecFactory(decodingScheme)
	decoder := decoderCodecFactory.UniversalDecoder(v1beta1.SchemeGroupVersion)
	err := runtime.DecodeInto(decoder, bytes, obj)
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	return obj
}

func encodeToYaml(obj runtime.Object) string {

	y := printers.YAMLPrinter{}
	b := bytes.Buffer{}
	_ = y.PrintObj(obj, &b)
	return b.String()
}

func streamRemoteYamlToTektonObj(url string, obj runtime.Object, ta *testArgs) runtime.Object {
	resp, err := http.Get(url) //#nosec G107
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	defer func(Body io.ReadCloser) {
		_ = Body.Close()
	}(resp.Body)
	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		debugAndFailTest(ta, err.Error())
	}
	return decodeBytesToTektonObjbytes(bytes, obj, ta)
}

func innerDumpPod(req *rest.Request, baseDirectory, localDirectory, podName, containerName string) error {
	var readCloser io.ReadCloser
	var err error
	readCloser, err = req.Stream(context.TODO())
	if err != nil {
		print(fmt.Sprintf("error getting pod logs for container %s: %s", containerName, err.Error()))
		return err
	}
	defer readCloser.Close()
	var b []byte
	b, err = io.ReadAll(readCloser)
	if err != nil {
		print(fmt.Sprintf("error reading pod stream %s", err.Error()))
		return err
	}
	directory := baseDirectory + "/" + localDirectory
	err = os.MkdirAll(directory, 0755) //#nosec G306 G301
	if err != nil {
		print(fmt.Sprintf("Failed to create artifact dir %s: %s", directory, err))
		return err
	}
	localPart := localDirectory + podName + containerName
	fileName := baseDirectory + "/" + localPart
	err = os.WriteFile(fileName, b, 0644) //#nosec G306
	if err != nil {
		print(fmt.Sprintf("Failed artifact dir %s: %s", directory, err))
		return err
	}
	return nil
}
