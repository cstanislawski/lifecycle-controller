package controller

import (
	"context"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func waitForRFC3339Tick() {
	nextSecond := time.Now().Truncate(time.Second).Add(time.Second)
	time.Sleep(time.Until(nextSecond) + 20*time.Millisecond)
}

func expectEventReason(ctx context.Context, objectName, reason string) {
	Eventually(func(g Gomega) {
		eventList := &corev1.EventList{}
		g.Expect(k8sClient.List(ctx, eventList, client.InNamespace(corev1.NamespaceDefault))).To(Succeed())
		found := false
		for _, event := range eventList.Items {
			if event.InvolvedObject.Name == objectName && event.Reason == reason {
				found = true
				break
			}
		}
		g.Expect(found).To(BeTrue())
	}, 10*time.Second, 250*time.Millisecond).Should(Succeed())
}
