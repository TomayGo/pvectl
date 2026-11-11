package k8s

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	plog "github.com/TomayGo/pvectl/internal/log"
)

func nodeWithReady(name string, status corev1.ConditionStatus, reason string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientMemory"},
			{Type: corev1.NodeReady, Status: status, Reason: reason},
		}},
	}
}

// waitClient answers each Get of the node from the list, the last entry
// repeating; a nil entry is "not found".
func waitClient(answers ...*corev1.Node) (*Client, *int) {
	cs := fake.NewSimpleClientset()
	calls := 0
	cs.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		n := answers[min(calls, len(answers)-1)]
		calls++
		if n == nil {
			return false, nil, nil // fall through to the tracker: not found
		}
		return true, n, nil
	})
	return NewForClientset(cs, plog.Discard(), false), &calls
}

// WaitReady asks every 5 s. A node that is missing or not Ready is asked about
// again; the deadline is checked after asking.
func TestWaitReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls := waitClient(nil, nodeWithReady("talos-cp-1", corev1.ConditionFalse, "KubeletNotReady"),
			nodeWithReady("talos-cp-1", corev1.ConditionTrue, "KubeletReady"))
		start := time.Now()
		if err := c.WaitReady(context.Background(), "talos-cp-1", time.Minute); err != nil {
			t.Fatal(err)
		}
		if *calls != 3 || time.Since(start) != 10*time.Second {
			t.Errorf("%d request(s) over %s, want 3 over 10s", *calls, time.Since(start))
		}
	})
}

// The timeout says what the node last reported, or why it could not be read.
func TestWaitReadyTimesOutWithTheLastAnswer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, calls := waitClient(nodeWithReady("talos-cp-1", corev1.ConditionUnknown, "NodeStatusUnknown"))
		start := time.Now()
		err := c.WaitReady(context.Background(), "talos-cp-1", 12*time.Second)
		if err == nil || err.Error() != "node talos-cp-1 not Ready after 12s (last: Unknown/NodeStatusUnknown)" {
			t.Errorf("error %v", err)
		}
		if *calls != 4 || time.Since(start) != 15*time.Second {
			t.Errorf("%d request(s) over %s, want 4 over 15s", *calls, time.Since(start))
		}
	})
	synctest.Test(t, func(t *testing.T) {
		c, _ := waitClient(nil)
		err := c.WaitReady(context.Background(), "talos-cp-1", 0)
		if err == nil || err.Error() != `node talos-cp-1 not Ready after 0s (last: nodes "talos-cp-1" not found)` {
			t.Errorf("error %v", err)
		}
	})
}

func TestWaitReadyHonoursCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := waitClient(nil)
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		defer cancel()
		if err := c.WaitReady(ctx, "talos-cp-1", time.Minute); err != context.DeadlineExceeded {
			t.Errorf("error %v", err)
		}
	})
}
