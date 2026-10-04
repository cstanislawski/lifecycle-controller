package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestLifecycleScopeAndRecoveryEnvtest(t *testing.T) {
	useExisting := false
	environment := &envtest.Environment{
		UseExistingCluster: &useExisting,
		CRDs: []*apiextensionsv1.CustomResourceDefinition{
			lifecycleTestCRD("workloads", "Deployment", apiextensionsv1.NamespaceScoped),
		},
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop test API server: %v", err)
		}
	})
	restConfig, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := eventsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	apiClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	const namespace = "team-a"
	lifecycleTestCreate(t, apiClient, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})

	newWorkload := func(name string, annotations map[string]string, withTemplate bool) *unstructured.Unstructured {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "example.test", Version: "v1", Kind: "Deployment"})
		obj.SetName(name)
		obj.SetNamespace(namespace)
		obj.SetAnnotations(annotations)
		if withTemplate {
			setTemplate(t, obj)
		}
		lifecycleTestCreate(t, apiClient, obj)
		return obj
	}
	newNativeWorkload := func(kind, name string, annotations map[string]string) *unstructured.Unstructured {
		obj := testDeployment(name, annotations)
		obj.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind(kind))
		obj.SetNamespace(namespace)
		obj.SetUID("")
		obj.SetResourceVersion("")
		setTemplate(t, obj)
		if err := unstructured.SetNestedStringMap(obj.Object, map[string]string{"app": "managed-by"}, "spec", "selector", "matchLabels"); err != nil {
			t.Fatal(err)
		}
		if kind == "StatefulSet" {
			if err := unstructured.SetNestedField(obj.Object, name, "spec", "serviceName"); err != nil {
				t.Fatal(err)
			}
		}
		lifecycleTestCreate(t, apiClient, obj)
		return obj
	}
	daemonSet := newNativeWorkload("DaemonSet", "one-time-daemonset", map[string]string{RestartAtAnnotation: "2000-01-01T00:00:00Z"})
	statefulSet := newNativeWorkload("StatefulSet", "one-time-statefulset", map[string]string{RestartAtAnnotation: "2000-01-01T00:00:00Z"})
	stateRepair := newNativeWorkload("Deployment", "state-repair", map[string]string{RestartEveryAnnotation: "1m", LastRestartTimestamp: "invalid"})
	customRestart := newWorkload("custom-restart", map[string]string{RestartAfterAnnotation: "1s"}, true)
	customDeletion := newWorkload("custom-deletion", map[string]string{DeleteAtAnnotation: "2000-01-01T00:00:00Z"}, false)
	now := time.Date(2026, time.October, 3, 12, 0, 0, 0, time.UTC)
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme: scheme, Logger: logr.Discard(), Metrics: metricsserver.Options{BindAddress: "0"},
		// The package also runs a separate manager with the same controller name.
		Controller: config.Controller{SkipNameValidation: new(true)},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &LifecycleReconciler{
		Client: manager.GetClient(), Scheme: scheme, now: func() time.Time { return now },
		Config: ScopeConfig{
			WatchResources: []string{
				"namespaces", "workloads.example.test",
				"deployments.apps", "statefulsets.apps", "daemonsets.apps",
			},
			WatchNamespaces: []string{namespace},
		},
	}
	if err := r.SetupWithManager(manager); err != nil {
		t.Fatal(err)
	}
	ctx, cancelManager := context.WithCancel(t.Context())
	done := make(chan error, 1)
	t.Cleanup(func() {
		cancelManager()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("stop test manager: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("test manager did not stop")
		}
	})
	go func() { done <- manager.Start(ctx) }()
	ready := r.ReadinessCheck(nil, false)
	eventually(t, 10*time.Second, func() bool { return ready(nil) == nil })

	for _, native := range []*unstructured.Unstructured{daemonSet, statefulSet} {
		lifecycleTestWaitForRestart(t, apiClient, native)
	}
	eventually(t, 10*time.Second, func() bool {
		return apierrors.IsNotFound(apiClient.Get(t.Context(), client.ObjectKeyFromObject(customDeletion), customDeletion.DeepCopy()))
	})

	for _, failure := range []struct{ name, reason string }{
		{name: stateRepair.GetName(), reason: "InvalidState"},
		{name: customRestart.GetName(), reason: "UnsupportedRestartKind"},
	} {
		lifecycleTestWaitForWarning(t, apiClient, client.ObjectKey{Namespace: namespace, Name: failure.name}, failure.reason)
	}
	annotations := stateRepair.GetAnnotations()
	annotations[LastRestartTimestamp] = now.Add(-2 * time.Minute).Format(time.RFC3339)
	stateRepair.SetAnnotations(annotations)
	if err := apiClient.Update(t.Context(), stateRepair); err != nil {
		t.Fatal(err)
	}
	lifecycleTestWaitForRestart(t, apiClient, stateRepair)

	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		current := customRestart.DeepCopy()
		if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(customRestart), current); err != nil {
			t.Fatalf("custom %s was removed: %v", customRestart.GetKind(), err)
		}
		if current.GetResourceVersion() != customRestart.GetResourceVersion() {
			t.Fatalf("custom %s %s changed unexpectedly", customRestart.GetKind(), customRestart.GetName())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lifecycleTestCRD(plural, kind string, scope apiextensionsv1.ResourceScope) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".example.test"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.test", Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Kind: kind}, Scope: scope,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: new(true),
				}},
			}},
		},
	}
}

func lifecycleTestCreate(t *testing.T, apiClient client.Client, obj client.Object) {
	t.Helper()
	if err := apiClient.Create(t.Context(), obj); err != nil {
		t.Fatalf("create %T: %v", obj, err)
	}
}

func lifecycleTestWaitForWarning(t *testing.T, apiClient client.Client, key client.ObjectKey, reason string) {
	t.Helper()
	eventually(t, 10*time.Second, func() bool {
		warnings := &eventsv1.EventList{}
		if err := apiClient.List(t.Context(), warnings, client.InNamespace(key.Namespace)); err != nil {
			return false
		}
		for _, warning := range warnings.Items {
			if warning.Regarding.Name == key.Name && warning.Reason == reason {
				return true
			}
		}
		return false
	})
}

func lifecycleTestWaitForRestart(t *testing.T, apiClient client.Client, obj *unstructured.Unstructured) {
	t.Helper()
	eventually(t, 10*time.Second, func() bool {
		if err := apiClient.Get(t.Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
			return false
		}
		marker, _, err := unstructured.NestedString(obj.Object, "spec", "template", "metadata", "annotations", RestartedAtTemplate)
		return err == nil && marker != "" && obj.GetAnnotations()[RestartAtAnnotation] == ""
	})
}
