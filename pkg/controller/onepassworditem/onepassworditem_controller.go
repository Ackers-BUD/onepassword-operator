package onepassworditem

import (
	"context"
	"fmt"

	onepasswordv1 "github.com/1Password/onepassword-operator/pkg/apis/onepassword/v1"
	kubeSecrets "github.com/1Password/onepassword-operator/pkg/kubernetessecrets"
	"github.com/1Password/onepassword-operator/pkg/onepassword"
	op "github.com/1Password/onepassword-operator/pkg/onepassword"
	"github.com/1Password/onepassword-operator/pkg/utils"

	"github.com/1Password/connect-sdk-go/connect"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	kubeClient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_onepassworditem")
var finalizer = "onepassword.com/finalizer.secret"

func Add(mgr manager.Manager, opConnectClient connect.Client) error {
	return add(mgr, newReconciler(mgr, opConnectClient))
}

func newReconciler(mgr manager.Manager, opConnectClient connect.Client) *ReconcileOnePasswordItem {
	return &ReconcileOnePasswordItem{
		kubeClient:      mgr.GetClient(),
		scheme:          mgr.GetScheme(),
		opConnectClient: opConnectClient,
	}
}

func add(mgr manager.Manager, r reconcile.Reconciler) error {
	c, err := controller.New("onepassworditem-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource OnePasswordItem
	err = c.Watch(&source.Kind{Type: &onepasswordv1.OnePasswordItem{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	return nil
}

var _ reconcile.Reconciler = &ReconcileOnePasswordItem{}

type ReconcileOnePasswordItem struct {
	kubeClient      kubeClient.Client
	scheme          *runtime.Scheme
	opConnectClient connect.Client
}

func (r *ReconcileOnePasswordItem) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&onepasswordv1.OnePasswordItem{}).
		Complete(r)
}

func (r *ReconcileOnePasswordItem) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling OnePasswordItem")

	onepassworditem := &onepasswordv1.OnePasswordItem{}
	err := r.kubeClient.Get(context.Background(), request.NamespacedName, onepassworditem)
	if err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// If the deployment is not being deleted
	if onepassworditem.ObjectMeta.DeletionTimestamp.IsZero() {
		// Adds a finalizer to the deployment if one does not exist.
		// This is so we can handle cleanup of associated secrets properly
		if !utils.ContainsString(onepassworditem.ObjectMeta.Finalizers, finalizer) {
			onepassworditem.ObjectMeta.Finalizers = append(onepassworditem.ObjectMeta.Finalizers, finalizer)
			if err := r.kubeClient.Update(context.Background(), onepassworditem); err != nil {
				return reconcile.Result{}, err
			}
		}

		// Handles creation or updating secrets for deployment if needed
		err := r.HandleOnePasswordItem(onepassworditem, request)
		if updateStatusErr := r.updateStatus(onepassworditem, err); updateStatusErr != nil {
			return reconcile.Result{}, fmt.Errorf("cannot update status: %s", updateStatusErr)
		}
		return reconcile.Result{}, err
	}
	// If one password finalizer exists then we must cleanup associated secrets
	if utils.ContainsString(onepassworditem.ObjectMeta.Finalizers, finalizer) {

		// Delete associated kubernetes secret
		if err = r.cleanupKubernetesSecret(onepassworditem); err != nil {
			return reconcile.Result{}, err
		}

		// Remove finalizer now that cleanup is complete
		if err := r.removeFinalizer(onepassworditem); err != nil {
			return reconcile.Result{}, err
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileOnePasswordItem) removeFinalizer(onePasswordItem *onepasswordv1.OnePasswordItem) error {
	onePasswordItem.ObjectMeta.Finalizers = utils.RemoveString(onePasswordItem.ObjectMeta.Finalizers, finalizer)
	if err := r.kubeClient.Update(context.Background(), onePasswordItem); err != nil {
		return err
	}
	return nil
}

func (r *ReconcileOnePasswordItem) cleanupKubernetesSecret(onePasswordItem *onepasswordv1.OnePasswordItem) error {
	kubernetesSecret := &corev1.Secret{}
	kubernetesSecret.ObjectMeta.Name = onePasswordItem.Name
	kubernetesSecret.ObjectMeta.Namespace = onePasswordItem.Namespace

	r.kubeClient.Delete(context.Background(), kubernetesSecret)
	if err := r.kubeClient.Delete(context.Background(), kubernetesSecret); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *ReconcileOnePasswordItem) removeOnePasswordFinalizerFromOnePasswordItem(opSecret *onepasswordv1.OnePasswordItem) error {
	opSecret.ObjectMeta.Finalizers = utils.RemoveString(opSecret.ObjectMeta.Finalizers, finalizer)
	return r.kubeClient.Update(context.Background(), opSecret)
}

func (r *ReconcileOnePasswordItem) HandleOnePasswordItem(resource *onepasswordv1.OnePasswordItem, request reconcile.Request) error {
	secretName := resource.GetName()
	labels := resource.Labels
	secretType := resource.Type
	autoRestart := resource.Annotations[op.RestartDeploymentsAnnotation]

	item, err := onepassword.GetOnePasswordItemByPath(r.opConnectClient, resource.Spec.ItemPath)
	if err != nil {
		return fmt.Errorf("Failed to retrieve item: %v", err)
	}

	// Create owner reference.
	gvk, err := apiutil.GVKForObject(resource, r.scheme)
	if err != nil {
		return fmt.Errorf("could not to retrieve group version kind: %v", err)
	}
	ownerRef := &metav1.OwnerReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       resource.GetName(),
		UID:        resource.GetUID(),
	}

	return kubeSecrets.CreateKubernetesSecretFromItem(r.kubeClient, secretName, resource.Namespace, item, autoRestart, labels, secretType, ownerRef)
}

func (r *ReconcileOnePasswordItem) updateStatus(resource *onepasswordv1.OnePasswordItem, err error) error {
	existingCondition := findCondition(resource.Status.Conditions, onepasswordv1.OnePasswordItemReady)
	updatedCondition := existingCondition
	if err != nil {
		updatedCondition.Message = err.Error()
		updatedCondition.Status = metav1.ConditionFalse
	} else {
		updatedCondition.Message = ""
		updatedCondition.Status = metav1.ConditionTrue
	}

	if existingCondition.Status != updatedCondition.Status {
		updatedCondition.LastTransitionTime = metav1.Now()
	}

	resource.Status.Conditions = []onepasswordv1.OnePasswordItemCondition{updatedCondition}
	return r.kubeClient.Status().Update(context.Background(), resource)
}

func findCondition(conditions []onepasswordv1.OnePasswordItemCondition, t onepasswordv1.OnePasswordItemConditionType) onepasswordv1.OnePasswordItemCondition {
	for _, c := range conditions {
		if c.Type == t {
			return c
		}
	}
	return onepasswordv1.OnePasswordItemCondition{
		Type:   t,
		Status: metav1.ConditionUnknown,
	}
}
