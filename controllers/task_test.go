package controllers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/tinkerbell/rufio/api/v1alpha1"
	"github.com/tinkerbell/rufio/controllers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func getAction(s string) v1alpha1.Action {
	switch s {
	case "PowerOn":
		return v1alpha1.Action{PowerAction: v1alpha1.PowerOn.Ptr()}
	case "HardOff":
		return v1alpha1.Action{PowerAction: v1alpha1.PowerHardOff.Ptr()}
	case "SoftOff":
		return v1alpha1.Action{PowerAction: v1alpha1.PowerSoftOff.Ptr()}
	case "BootPXE":
		return v1alpha1.Action{OneTimeBootDeviceAction: &v1alpha1.OneTimeBootDeviceAction{Devices: []v1alpha1.BootDevice{v1alpha1.PXE}}}
	case "VirtualMedia":
		return v1alpha1.Action{VirtualMediaAction: &v1alpha1.VirtualMediaAction{MediaURL: "http://example.com/image.iso", Kind: v1alpha1.VirtualMediaCD}}
	default:
		return v1alpha1.Action{}
	}
}

func TestTaskReconcile(t *testing.T) {
	tests := map[string]struct {
		taskName   string
		action     v1alpha1.Action
		provider   *testProvider
		shouldErr  bool
		timeoutErr bool
	}{
		"success power on":           {taskName: "PowerOn", action: getAction("PowerOn"), provider: &testProvider{Powerstate: "on", PowerSetOK: true}},
		"success hard off":           {taskName: "HardOff", action: getAction("HardOff"), provider: &testProvider{Powerstate: "off", PowerSetOK: true}},
		"success soft off":           {taskName: "SoftOff", action: getAction("SoftOff"), provider: &testProvider{Powerstate: "off", PowerSetOK: true}},
		"success boot pxe":           {taskName: "BootPXE", action: getAction("BootPXE"), provider: &testProvider{BootdeviceOK: true}},
		"success virtual media":      {taskName: "VirtualMedia", action: getAction("VirtualMedia"), provider: &testProvider{VirtualMediaOK: true}},
		"failure on bmc open":        {taskName: "PowerOn", action: getAction("PowerOn"), provider: &testProvider{ErrOpen: errors.New("failed to open")}, shouldErr: true},
		"failure on bmc power on":    {taskName: "PowerOn", action: getAction("PowerOn"), provider: &testProvider{ErrPowerStateSet: errors.New("failed to set power state")}, shouldErr: true},
		"failure on set boot device": {taskName: "BootPXE", action: getAction("BootPXE"), provider: &testProvider{ErrBootDeviceSet: errors.New("failed to set boot device")}, shouldErr: true},
		"failure on virtual media":   {taskName: "VirtualMedia", action: getAction("VirtualMedia"), provider: &testProvider{ErrVirtualMediaInsert: errors.New("failed to set virtual media")}, shouldErr: true},
		"failure timeout":            {taskName: "PowerOn", action: getAction("PowerOn"), provider: &testProvider{Powerstate: "off", PowerSetOK: true}, timeoutErr: true},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			secret := createSecret()
			task := createTask(tt.taskName, tt.action, secret)
			cluster := createKubeClientWithObjects(task, secret)
			reconciler := controllers.NewTaskReconciler(cluster, newTestClient(tt.provider))
			request := reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: task.Namespace,
					Name:      task.Name,
				},
			}

			result, err := reconciler.Reconcile(context.Background(), request)
			if !tt.shouldErr && err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			if tt.shouldErr && err == nil {
				t.Fatalf("expected err, got: %v", err)
			}
			if tt.shouldErr {
				return
			}
			if diff := cmp.Diff(result, ctrl.Result{}); diff != "" {
				t.Fatalf("expected no diff, got: %v", diff)
			}

			var retrieved v1alpha1.Task
			if err = cluster.Get(context.Background(), request.NamespacedName, &retrieved); err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			// TODO: g.Expect(retrieved.Status.StartTime.Unix()).To(gomega.BeNumerically("~", time.Now().Unix(), 2))
			if !retrieved.Status.CompletionTime.IsZero() {
				t.Fatalf("expected completion time to be zero, got: %v", retrieved.Status.CompletionTime)
			}
			if len(retrieved.Status.Conditions) != 0 {
				t.Fatalf("expected no conditions, got: %v", retrieved.Status.Conditions)
			}

			// Timeout check
			if tt.timeoutErr {
				expired := metav1.NewTime(retrieved.Status.StartTime.Add(-time.Hour))
				retrieved.Status.StartTime = &expired
				if err = cluster.Update(context.Background(), &retrieved); err != nil {
					t.Fatalf("expected nil err, got: %v", err)
				}

				result, err = reconciler.Reconcile(context.Background(), request)
				if err == nil {
					t.Fatalf("expected err, got: %v", err)
				}
				if diff := cmp.Diff(result, ctrl.Result{}); diff != "" {
					t.Fatalf("expected no diff, got: %v", diff)
				}
				return
			}

			// Ensure re-reconciling a task does sends it into a success state.
			result, err = reconciler.Reconcile(context.Background(), request)
			if err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			if diff := cmp.Diff(result, reconcile.Result{}); diff != "" {
				t.Fatalf("expected no diff, got: %v", diff)
			}

			err = cluster.Get(context.Background(), request.NamespacedName, &retrieved)
			if err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			// TODO: g.Expect(retrieved.Status.CompletionTime.Unix()).To(gomega.BeNumerically("~", time.Now().Unix(), 2))
			if len(retrieved.Status.Conditions) != 1 {
				t.Fatalf("expected 1 condition, got: %v", retrieved.Status.Conditions)
			}
			if retrieved.Status.Conditions[0].Type != v1alpha1.TaskCompleted {
				t.Fatalf("expected condition type to be %s, got: %s", v1alpha1.TaskCompleted, retrieved.Status.Conditions[0].Type)
			}
			if retrieved.Status.Conditions[0].Status != v1alpha1.ConditionTrue {
				t.Fatalf("expected condition status to be %s, got: %s", v1alpha1.ConditionTrue, retrieved.Status.Conditions[0].Status)
			}

			var retrieved2 v1alpha1.Task
			err = cluster.Get(context.Background(), request.NamespacedName, &retrieved2)
			if err != nil {
				t.Fatalf("expected nil err, got: %v", err)
			}
			if diff := cmp.Diff(retrieved2, retrieved); diff != "" {
				t.Fatalf("expected no diff, got: %v", diff)
			}
		})
	}
}

func createTask(name string, action v1alpha1.Action, secret *corev1.Secret) *v1alpha1.Task {
	return &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: v1alpha1.TaskSpec{
			Task: action,
			Connection: v1alpha1.Connection{
				Host: "host",
				Port: 22,
				AuthSecretRef: corev1.SecretReference{
					Name:      secret.Name,
					Namespace: secret.Namespace,
				},
			},
		},
	}
}