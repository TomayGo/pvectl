package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func fakeClient(objs ...runtime.Object) *Client {
	c := &Client{client: fake.NewSimpleClientset(objs...)}
	c.once.Do(func() {})
	return c
}

func cnpgPod(name, node, role string, ready bool) *corev1.Pod {
	st := corev1.ConditionFalse
	if ready {
		st = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "monitoring", Labels: map[string]string{
			"cnpg.io/cluster": "grafana-db", "cnpg.io/instanceRole": role,
		}},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: st}}},
	}
}

func node(name string, unschedulable bool) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: corev1.NodeSpec{Unschedulable: unschedulable}}
}

// The CNPG primary PDB, exactly as it exists in the cluster: it selects the
// primary only and therefore always allows 0 disruptions.
var primaryPDB = &policyv1.PodDisruptionBudget{
	ObjectMeta: metav1.ObjectMeta{Name: "grafana-db-primary", Namespace: "monitoring"},
	Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
		"cnpg.io/cluster": "grafana-db", "cnpg.io/instanceRole": "primary",
	}}},
	Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: 0, CurrentHealthy: 1, DesiredHealthy: 1},
}

func TestCNPGPrimaryWithHealthyReplicaIsAHandover(t *testing.T) {
	primary := cnpgPod("grafana-db-2", "talos-cp-1", "primary", true)
	c := fakeClient(primary, cnpgPod("grafana-db-1", "talos-cp-2", "replica", true),
		node("talos-cp-2", false), primaryPDB)
	blocked, handovers, err := c.pdbBlockers(context.Background(), "talos-cp-1", []corev1.Pod{*primary})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 0 || len(handovers) != 1 {
		t.Fatalf("blocked=%v handovers=%v", blocked, handovers)
	}
}

// Without somewhere to switch to, the PDB really does hang the drain.
func TestCNPGPrimaryWithoutUsableReplicaStaysBlocked(t *testing.T) {
	primary := cnpgPod("grafana-db-2", "talos-cp-1", "primary", true)
	cases := map[string][]runtime.Object{
		"no replica":           {primary, node("talos-cp-2", false), primaryPDB},
		"replica not ready":    {primary, cnpgPod("grafana-db-1", "talos-cp-2", "replica", false), node("talos-cp-2", false), primaryPDB},
		"replica cordoned":     {primary, cnpgPod("grafana-db-1", "talos-cp-2", "replica", true), node("talos-cp-2", true), primaryPDB},
		"replica on same node": {primary, cnpgPod("grafana-db-1", "talos-cp-1", "replica", true), node("talos-cp-1", false), primaryPDB},
	}
	for name, objs := range cases {
		c := fakeClient(objs...)
		blocked, handovers, err := c.pdbBlockers(context.Background(), "talos-cp-1", []corev1.Pod{*primary})
		if err != nil {
			t.Fatal(err)
		}
		if len(blocked) != 1 || len(handovers) != 0 {
			t.Errorf("%s: blocked=%v handovers=%v", name, blocked, handovers)
		}
	}
}

// The Prometheus as it was until 2026-09-28 (no storage) and as it is now.
func TestEphemeralFromList(t *testing.T) {
	raw := []byte(`{"items":[
	 {"metadata":{"namespace":"monitoring","name":"old"},"spec":{"replicas":2,"retention":"15d"}},
	 {"metadata":{"namespace":"monitoring","name":"empty"},"spec":{"storage":{}}},
	 {"metadata":{"namespace":"monitoring","name":"kube-prometheus-stack-prometheus"},
	  "spec":{"storage":{"volumeClaimTemplate":{"spec":{"storageClassName":"ceph-rbd"}}}}}]}`)
	got, err := ephemeralFromList(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "monitoring/empty" || got[1] != "monitoring/old" {
		t.Fatalf("got %v", got)
	}
}
