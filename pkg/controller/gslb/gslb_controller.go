package gslb

import (
	"context"
	"time"

	k8gbv1beta1 "github.com/AbsaOSS/k8gb/pkg/apis/k8gb/v1beta1"
	"github.com/AbsaOSS/k8gb/pkg/controller/gslb/internal/depresolver"
	externaldns "github.com/kubernetes-incubator/external-dns/endpoint"
	corev1 "k8s.io/api/core/v1"
	v1beta1 "k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	types "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

var log = logf.Log.WithName("controller_gslb")

/**
* USER ACTION REQUIRED: This is a scaffold file intended for the user to modify with their own Controller
* business logic.  Delete these comments after modifying this file.*
 */

// Add creates a new Gslb Controller and adds it to the Manager. The Manager will set fields on the Controller
// and Start it when the Manager is Started.
func Add(mgr manager.Manager) error {
	reconciler, err := newReconciler(mgr)
	if err != nil {
		return err
	}
	return add(mgr, reconciler)
}

// newReconciler returns a new reconcile.Reconciler
func newReconciler(mgr manager.Manager) (reconcile.Reconciler,error) {
	var err error
	reconciler := new(ReconcileGslb)
	reconciler.client = mgr.GetClient()
	reconciler.scheme = mgr.GetScheme()
	reconciler.depResolver = depresolver.NewDependencyResolver(context.TODO(), reconciler.client)
	reconciler.config, err = reconciler.depResolver.ResolveOperatorConfig()
	if err != nil {
		log.Error(err,"reading config env variables")
		return nil,err
	}
	return reconciler,nil
}

// add adds a new Controller to mgr with r as the reconcile.Reconciler
func add(mgr manager.Manager, r reconcile.Reconciler) error {
	// Create a new controller
	c, err := controller.New("gslb-controller", mgr, controller.Options{Reconciler: r})
	if err != nil {
		return err
	}

	// Watch for changes to primary resource Gslb
	err = c.Watch(&source.Kind{Type: &k8gbv1beta1.Gslb{}}, &handler.EnqueueRequestForObject{})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource Ingress
	err = c.Watch(&source.Kind{Type: &v1beta1.Ingress{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &k8gbv1beta1.Gslb{},
	})
	if err != nil {
		return err
	}

	// Watch for changes to secondary resource DNSEndpoint
	err = c.Watch(&source.Kind{Type: &externaldns.DNSEndpoint{}}, &handler.EnqueueRequestForOwner{
		IsController: true,
		OwnerType:    &k8gbv1beta1.Gslb{},
	})
	if err != nil {
		return err
	}

	// Figure out Gslb resource name to Reconcile when non controlled Endpoint is updated
	mapFn := handler.ToRequestsFunc(
		func(a handler.MapObject) []reconcile.Request {
			gslbList := &k8gbv1beta1.GslbList{}
			opts := []client.ListOption{
				client.InNamespace(a.Meta.GetNamespace()),
			}
			c := mgr.GetClient()
			err := c.List(context.TODO(), gslbList, opts...)
			if err != nil {
				log.Info("Can't fetch gslb objects")
				return nil
			}
			gslbName := ""
			for _, gslb := range gslbList.Items {
				for _, rule := range gslb.Spec.Ingress.Rules {
					for _, path := range rule.HTTP.Paths {
						if path.Backend.ServiceName == a.Meta.GetName() {
							gslbName = gslb.Name
						}
					}
				}
			}
			if len(gslbName) > 0 {
				return []reconcile.Request{
					{NamespacedName: types.NamespacedName{
						Name:      gslbName,
						Namespace: a.Meta.GetNamespace(),
					}},
				}
			}
			return nil
		})

	// Watch for Endpoints that are not controlled directly
	err = c.Watch(
		&source.Kind{Type: &corev1.Endpoints{}},
		&handler.EnqueueRequestsFromMapFunc{
			ToRequests: mapFn,
		},
	)
	if err != nil {
		return err
	}

	return nil
}

// blank assignment to verify that ReconcileGslb implements reconcile.Reconciler
var _ reconcile.Reconciler = &ReconcileGslb{}

// ReconcileGslb reconciles a Gslb object
type ReconcileGslb struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	client      client.Client
	scheme      *runtime.Scheme
	config      *depresolver.Config
	depResolver *depresolver.DependencyResolver
}

const gslbFinalizer = "finalizer.k8gb.absa.oss"

// Reconcile reads that state of the cluster for a Gslb object and makes changes based on the state read
// and what is in the Gslb.Spec
// Note:
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (r *ReconcileGslb) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace, "Request.Name", request.Name)
	reqLogger.Info("Reconciling Gslb")

	// Fetch the Gslb instance
	gslb := &k8gbv1beta1.Gslb{}
	err := r.client.Get(context.TODO(), request.NamespacedName, gslb)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	}

	var result *reconcile.Result

	err = r.depResolver.ResolveGslbSpec(gslb)
	if err != nil {
		log.Error(err,"resolving spec.strategy")
		return reconcile.Result{}, err
	}
	// == Finalizer business ==

	// Check if the Gslb instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isGslbMarkedToBeDeleted := gslb.GetDeletionTimestamp() != nil
	if isGslbMarkedToBeDeleted {
		if contains(gslb.GetFinalizers(), gslbFinalizer) {
			// Run finalization logic for gslbFinalizer. If the
			// finalization logic fails, don't remove the finalizer so
			// that we can retry during the next reconciliation.
			if err := r.finalizeGslb(gslb); err != nil {
				return reconcile.Result{}, err
			}

			// Remove gslbFinalizer. Once all finalizers have been
			// removed, the object will be deleted.
			gslb.SetFinalizers(remove(gslb.GetFinalizers(), gslbFinalizer))
			err := r.client.Update(context.TODO(), gslb)
			if err != nil {
				return reconcile.Result{}, err
			}
		}
		return reconcile.Result{}, nil
	}

	// Add finalizer for this CR
	if !contains(gslb.GetFinalizers(), gslbFinalizer) {
		if err := r.addFinalizer(gslb); err != nil {
			return reconcile.Result{}, err
		}
	}

	// == Ingress ==========
	ingress, err := r.gslbIngress(gslb)
	if err != nil {
		// Requeue the request
		return reconcile.Result{}, err
	}

	result, err = r.ensureIngress(
		request,
		gslb,
		ingress)
	if result != nil {
		return *result, err
	}

	// == external-dns dnsendpoints CRs ==
	dnsEndpoint, err := r.gslbDNSEndpoint(gslb)
	if err != nil {
		// Requeue the request
		return reconcile.Result{}, err
	}

	result, err = r.ensureDNSEndpoint(
		request,
		gslb,
		dnsEndpoint)
	if result != nil {
		return *result, err
	}

	// == handle delegated zone in Edge DNS

	result, err = r.configureZoneDelegation(gslb)
	if result != nil {
		return *result, err
	}

	// == Status =
	err = r.updateGslbStatus(gslb)
	if err != nil {
		// Requeue the request
		return reconcile.Result{}, err
	}

	// == Finish ==========
	// Everything went fine, requeue after some time to catch up
	// with external Gslb status
	// TODO: potentially enhance with smarter reaction to external Event
	return reconcile.Result{RequeueAfter: time.Second * time.Duration(r.config.ReconcileRequeueSeconds)}, nil}
