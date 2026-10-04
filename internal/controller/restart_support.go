package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func supportsRestart(obj *unstructured.Unstructured) bool {
	gvk := obj.GetObjectKind().GroupVersionKind()
	if gvk.Group != appsv1.GroupName {
		return false
	}
	switch gvk.Kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		return true
	default:
		return false
	}
}

func hasRestartTemplate(obj *unstructured.Unstructured) bool {
	_, found, err := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template")
	return found && err == nil
}
