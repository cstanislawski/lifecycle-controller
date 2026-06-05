package policies

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestPolicyExamplesDecodeAsKubernetesObjects(t *testing.T) {
	files := []string{
		"gatekeeper-restrict-lifecycle-annotations.yaml",
		"kyverno-restrict-lifecycle-annotations.yaml",
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			objects := readObjects(t, file)
			if len(objects) == 0 {
				t.Fatalf("%s had no Kubernetes objects", file)
			}
			for _, obj := range objects {
				if obj.GetAPIVersion() == "" || obj.GetKind() == "" || obj.GetName() == "" {
					t.Fatalf("%s contains object without apiVersion, kind, or metadata.name: %#v", file, obj.Object)
				}
			}
		})
	}
}

func TestKyvernoPolicyGuardsLifecycleAnnotationWrites(t *testing.T) {
	policy := readObjects(t, "kyverno-restrict-lifecycle-annotations.yaml")[0]
	if policy.GetAPIVersion() != "kyverno.io/v1" || policy.GetKind() != "ClusterPolicy" {
		t.Fatalf("unexpected Kyverno object %s %s", policy.GetAPIVersion(), policy.GetKind())
	}

	background, found, err := unstructured.NestedBool(policy.Object, "spec", "background")
	if err != nil || !found || background {
		t.Fatalf("Kyverno policy must set spec.background=false, found=%v value=%v err=%v", found, background, err)
	}

	rules, found, err := unstructured.NestedSlice(policy.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 3 {
		t.Fatalf(
			"Kyverno policy must have three rules for create, update-change, update-removal; found=%v len=%d err=%v",
			found,
			len(rules),
			err,
		)
	}

	foundCreate := false
	foundUpdate := 0
	for _, rule := range rules {
		ruleMap, ok := rule.(map[string]interface{})
		if !ok {
			t.Fatalf("rule is not an object: %#v", rule)
		}
		name, _ := ruleMap["name"].(string)
		if strings.Contains(name, "create") {
			foundCreate = true
		}
		if strings.Contains(name, "update") {
			foundUpdate++
		}
		assertKyvernoRuleUsesWildcardMatch(t, ruleMap)
		assertKyvernoRuleExemptsController(t, ruleMap)
		assertKyvernoRuleEnforcesLifecyclePrefix(t, ruleMap)
	}

	if !foundCreate || foundUpdate != 2 {
		t.Fatalf("expected one create rule and two update rules, got create=%v update=%d", foundCreate, foundUpdate)
	}
}

func TestGatekeeperPolicyGuardsLifecycleAnnotationWrites(t *testing.T) {
	objects := readObjects(t, "gatekeeper-restrict-lifecycle-annotations.yaml")
	if len(objects) != 2 {
		t.Fatalf("Gatekeeper example must contain template and constraint, got %d objects", len(objects))
	}

	template := objects[0]
	if template.GetAPIVersion() != "templates.gatekeeper.sh/v1" || template.GetKind() != "ConstraintTemplate" {
		t.Fatalf("unexpected Gatekeeper template %s %s", template.GetAPIVersion(), template.GetKind())
	}

	schemaType, found, err := unstructured.NestedString(
		template.Object,
		"spec",
		"crd",
		"spec",
		"validation",
		"openAPIV3Schema",
		"type",
	)
	if err != nil || !found || schemaType != "object" {
		t.Fatalf(
			"Gatekeeper template must use structural schema type=object, found=%v value=%q err=%v",
			found,
			schemaType,
			err,
		)
	}

	targets, found, err := unstructured.NestedSlice(template.Object, "spec", "targets")
	if err != nil || !found || len(targets) != 1 {
		t.Fatalf("Gatekeeper template must include one target, found=%v len=%d err=%v", found, len(targets), err)
	}
	target, ok := targets[0].(map[string]interface{})
	if !ok {
		t.Fatalf("Gatekeeper target is not an object: %#v", targets[0])
	}
	rego, ok := target["rego"].(string)
	if !ok || rego == "" {
		t.Fatal("Gatekeeper template must include Rego source")
	}
	for _, required := range []string{
		`input.review.operation == "CREATE"`,
		`input.review.operation == "UPDATE"`,
		`startswith(key, "lifecycle.cezary.dev/")`,
		`input.parameters.allowedUsernames`,
		`input.parameters.allowedGroups`,
	} {
		if !strings.Contains(rego, required) {
			t.Fatalf("Gatekeeper Rego missing %q", required)
		}
	}

	constraint := objects[1]
	if constraint.GetAPIVersion() != "constraints.gatekeeper.sh/v1beta1" ||
		constraint.GetKind() != "K8sLifecycleAnnotationGuard" {
		t.Fatalf("unexpected Gatekeeper constraint %s %s", constraint.GetAPIVersion(), constraint.GetKind())
	}
	action, found, err := unstructured.NestedString(constraint.Object, "spec", "enforcementAction")
	if err != nil || !found || action != "deny" {
		t.Fatalf("Gatekeeper constraint must deny violations, found=%v value=%q err=%v", found, action, err)
	}
}

func assertKyvernoRuleUsesWildcardMatch(t *testing.T, rule map[string]interface{}) {
	t.Helper()
	text := stringify(rule["match"])
	if !strings.Contains(text, "*") {
		t.Fatalf("Kyverno rule %q must match wildcard resources", rule["name"])
	}
}

func assertKyvernoRuleExemptsController(t *testing.T, rule map[string]interface{}) {
	t.Helper()
	text := stringify(rule["exclude"])
	for _, required := range []string{
		"lifecycle-controller-annotation-admins",
		"lifecycle-controller-controller-manager",
		"lifecycle-controller",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("Kyverno rule %q missing exemption %q", rule["name"], required)
		}
	}
}

func assertKyvernoRuleEnforcesLifecyclePrefix(t *testing.T, rule map[string]interface{}) {
	t.Helper()
	validate := stringify(rule["validate"])
	for _, required := range []string{
		"Enforce",
		"foreach",
		"starts_with(element.key, 'lifecycle.cezary.dev/')",
	} {
		if !strings.Contains(validate, required) {
			t.Fatalf("Kyverno rule %q missing validation marker %q", rule["name"], required)
		}
	}
}

func readObjects(t *testing.T, file string) []*unstructured.Unstructured {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(file))
	if err != nil {
		t.Fatal(err)
	}

	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	objects := []*unstructured.Unstructured{}
	for {
		var raw map[string]interface{}
		err := decoder.Decode(&raw)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode %s: %v", file, err)
		}
		if len(raw) == 0 {
			continue
		}
		objects = append(objects, &unstructured.Unstructured{Object: raw})
	}
	return objects
}

func stringify(value interface{}) string {
	return strings.Join(strings.Fields(fmt.Sprintf("%#v", value)), " ")
}
