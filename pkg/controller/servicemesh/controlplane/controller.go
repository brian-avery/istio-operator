package controlplane

import (
	"context"
	"fmt"
	"io/ioutil"
	"strings"

	"github.com/ghodss/yaml"

	"github.com/go-logr/logr"
	"github.com/maistra/istio-operator/pkg/bootstrap"

	v1 "github.com/maistra/istio-operator/pkg/apis/maistra/v1"
	"github.com/maistra/istio-operator/pkg/controller/common"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	logf "sigs.k8s.io/controller-runtime/pkg/runtime/log"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_servicemeshcontrolplane")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new ControlPlane Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	operatorNamespace, err := common.GetOperatorNamespace()
	if err != nil {
		return err
	}
	return add(mgr, newReconciler(mgr, operatorNamespace))
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager, operatorNamespace string) reconcile.Reconciler {
	return &ReconcileControlPlane{
		ResourceManager: common.ResourceManager{
			Client:            mgr.GetClient(),
			PatchFactory:      common.NewPatchFactory(mgr.GetClient()),
			Log:               log,
			OperatorNamespace: operatorNamespace,
		},
		Scheme:  mgr.GetScheme(),
		Manager: mgr,
	}
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("servicemeshcontrolplane-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource ServiceMeshControlPlane
	// XXX: hack: remove predicate once old installation mechanism is removed
	err = c.Watch(&source.Kind{Type: &v1.ServiceMeshControlPlane{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// XXX: consider adding watches on created resources.  This would need to be
	// done in the reconciler, although I suppose we could hard code known types
	// (ServiceAccount, Service, ClusterRole, ClusterRoleBinding, Deployment,
	// ConfigMap, ValidatingWebhook, MutatingWebhook, MeshPolicy, DestinationRule,
	// Gateway, PodDisruptionBudget, HorizontalPodAutoscaler, Ingress, Route).
	// TODO(user): Modify this to be the types you create that are owned by the primary resource
	// Watch for changes to secondary resource Pods and requeue the owner ControlPlane
	// err = c.Watch(&source.Kind{Type: &corev1.Pod{}}, &handler.EnqueueRequestForOwner{
	// 	IsController: true,
	// 	OwnerType:    &v1.ControlPlane{},
	// })
	// if err != nil {
	// 	return err
	// }

	return nil
}

var _ reconcile.Reconciler = &ReconcileControlPlane{}

// ReconcileControlPlane reconciles a ServiceMeshControlPlane object
type ReconcileControlPlane struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	common.ResourceManager
	Scheme  *runtime.Scheme
	Manager manager.Manager
}

const (
	finalizer           = "istio-operator-ControlPlane"
	scmpTemplatePath    = "/etc/istio-operator/templates/"
	smcpDefaultTemplate = "maistra.yaml"
)

func getSMCPTemplate(name string) (*v1.ControlPlaneSpec, error) {
	if strings.Contains(name, "/") {
		return nil, fmt.Errorf("template name contains invalid character '/'")
	}

	templateContent, err := ioutil.ReadFile(scmpTemplatePath + name)
	if err != nil {
		return nil, err
	}

	var template v1.ServiceMeshControlPlane
	if err = yaml.Unmarshal(templateContent, &template); err != nil {
		return nil, fmt.Errorf("failed to parse template %s contents: %s", name, err)

	}
	return &template.Spec, nil
}

//TODO(bavery) -- use pointers?
func mergeValues(base map[string]interface{}, input map[string]interface{}) map[string]interface{} {
	if base == nil {
		base = make(map[string]interface{}, 0)
	}

	for key, value := range input {
		//if the key doesn't already exist, add it
		if _, exists := base[key]; !exists {
			base[key] = value
			continue
		}

		//at this point, key exists in both input and base. Is the value a map?
		//if it's not a map, replace whatever is in base with the value.
		inputAsMap, ok := value.(map[string]interface{})
		if !ok {
			base[key] = value
			continue
		}

		//at this point, we know the key exists in both base and input.
		//We also know that it's a map. Validate that it's a map and recurse again.
		baseKeyAsMap, ok := base[key].(map[string]interface{})
		if !ok {
			baseKeyAsMap = make(map[string]interface{}, 0)
		}
		base[key] = mergeValues(baseKeyAsMap, inputAsMap)
	}
	return base
}

func reconcileTemplates(smcp *v1.ControlPlaneSpec, visited map[string]struct{}) (*v1.ControlPlaneSpec, error) {
	if smcp.Template == "" {
		return smcp, nil
	}

	if _, ok := visited[smcp.Template]; ok {
		return nil, fmt.Errorf("Templates form cyclic dependency. Cannot proceed")
	}

	template, err := getSMCPTemplate(smcp.Template)
	if err != nil {
		return nil, err
	}

	visited[smcp.Template] = struct{}{}
	template, err = reconcileTemplates(template, visited)
	if err != nil {
		return nil, err
	}

	smcp.Istio = mergeValues(smcp.Istio, template.Istio)
	smcp.ThreeScale = mergeValues(smcp.ThreeScale, template.ThreeScale)
	return smcp, nil
}

func getServiceMeshControlPlane(reqLogger logr.Logger, smcp *v1.ServiceMeshControlPlane) (*v1.ServiceMeshControlPlane, error) {
	reqLogger.Info("Processing ServiceMeshControlPlane templates")
	//if a template is not set in the initial CP, assign the default one.
	if smcp.Spec.Template == "" {
		reqLogger.Info("Template not set. Loading defaults.")
		smcp.Spec.Template = smcpDefaultTemplate
	}

	smcpSpec, err := reconcileTemplates(&smcp.Spec, make(map[string]struct{}, 0))
	if err != nil {
		return smcp, err
	}
	smcp.Spec = *smcpSpec
	return smcp, nil

}

// Reconcile reads that state of the cluster for a ServiceMeshControlPlane object and makes changes based on the state read
// and what is in the ServiceMeshControlPlane.Spec
// TODO(user): Modify this Reconcile function to implement your Controller logic.  This example creates
// a Pod as an example
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileControlPlane) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Processing ServiceMeshControlPlane")

	// Fetch the ServiceMeshControlPlane instance
	instance := &v1.ServiceMeshControlPlane{}
	err := r.Client.Get(context.TODO(), request.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) || errors.IsGone(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			reqLogger.Info("ServiceMeshControlPlane deleted")
			return reconcile.Result{}, nil
		}
		// Error reading the object
		return reconcile.Result{}, err
	}

	smcp, err := getServiceMeshControlPlane(reqLogger, instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	reconciler := ControlPlaneReconciler{
		ReconcileControlPlane: r,
		Instance:              smcp,
		Status:                v1.NewControlPlaneStatus(),
		UpdateStatus: func() error {
			return r.Client.Status().Update(context.TODO(), instance)
		},
		NewOwnerRef: func(owner *v1.ServiceMeshControlPlane) *metav1.OwnerReference {
			return metav1.NewControllerRef(owner, v1.SchemeGroupVersion.WithKind("ServiceMeshControlPlane"))
		},
	}

	deleted := instance.GetDeletionTimestamp() != nil
	finalizers := instance.GetFinalizers()
	finalizerIndex := common.IndexOf(finalizers, finalizer)

	if deleted {
		if finalizerIndex < 0 {
			reqLogger.Info("ServiceMeshControlPlane deleted")
			return reconcile.Result{}, nil
		}
		reqLogger.Info("Deleting ServiceMeshControlPlane")
		result, err := reconciler.Delete()
		// XXX: for now, nuke the resources, regardless of errors
		finalizers = append(finalizers[:finalizerIndex], finalizers[finalizerIndex+1:]...)
		instance.SetFinalizers(finalizers)
		finalizerError := r.Client.Update(context.TODO(), instance)
		for retryCount := 0; errors.IsConflict(finalizerError) && retryCount < 5; retryCount++ {
			reqLogger.Info("conflict during finalizer removal, retrying")
			err := r.Client.Get(context.TODO(), request.NamespacedName, instance)
			if err != nil {
				reqLogger.Error(err, "Could not get ServiceMeshControlPlane")
				continue
			}
			finalizers = instance.GetFinalizers()
			finalizerIndex = common.IndexOf(finalizers, finalizer)
			finalizers = append(finalizers[:finalizerIndex], finalizers[finalizerIndex+1:]...)
			instance.SetFinalizers(finalizers)
			finalizerError = r.Client.Update(context.TODO(), instance)
		}
		if finalizerError != nil {
			reqLogger.Error(finalizerError, "error removing finalizer")
		}
		return result, err
	} else if finalizerIndex < 0 {
		reqLogger.V(1).Info("Adding finalizer", "finalizer", finalizer)
		finalizers = append(finalizers, finalizer)
		instance.SetFinalizers(finalizers)
		err = r.Client.Update(context.TODO(), instance)
		return reconcile.Result{}, err
	}

	if instance.GetGeneration() == instance.Status.ObservedGeneration &&
		instance.Status.GetCondition(v1.ConditionTypeReconciled).Status == v1.ConditionStatusTrue {
		reqLogger.Info("nothing to reconcile, generations match")
		return reconcile.Result{}, nil
	}

	reqLogger.Info("Reconciling ServiceMeshControlPlane")

	// Enusure CRDs are installed
	err = bootstrap.InstallCRDs(reconciler.Manager)
	if err != nil {
		reqLogger.Error(err, "Failed to install/update Istio CRDs")
		return reconcile.Result{}, err
	}

	return reconciler.Reconcile()
}
