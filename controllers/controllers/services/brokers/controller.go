/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package brokers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	korifiv1alpha1 "code.cloudfoundry.org/korifi/controllers/api/v1alpha1"
	"code.cloudfoundry.org/korifi/controllers/controllers/services/brokers/osbapi"
	"code.cloudfoundry.org/korifi/controllers/controllers/shared"
	"code.cloudfoundry.org/korifi/model/services"
	"code.cloudfoundry.org/korifi/tools"
	"code.cloudfoundry.org/korifi/tools/k8s"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type CatalogClient interface {
	GetCatalog(context.Context, *korifiv1alpha1.CFServiceBroker) (*osbapi.Catalog, error)
}

type Reconciler struct {
	k8sClient     client.Client
	catalogClient CatalogClient
	scheme        *runtime.Scheme
	log           logr.Logger
}

func NewReconciler(
	client client.Client,
	catalogClient CatalogClient,
	scheme *runtime.Scheme,
	log logr.Logger,
) *k8s.PatchingReconciler[korifiv1alpha1.CFServiceBroker, *korifiv1alpha1.CFServiceBroker] {
	serviceInstanceReconciler := Reconciler{k8sClient: client, catalogClient: catalogClient, scheme: scheme, log: log}
	return k8s.NewPatchingReconciler(log, client, &serviceInstanceReconciler)
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) *builder.Builder {
	return ctrl.NewControllerManagedBy(mgr).
		For(&korifiv1alpha1.CFServiceBroker{}).
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(r.secretToServiceBroker),
		)
}

func (r *Reconciler) secretToServiceBroker(ctx context.Context, o client.Object) []reconcile.Request {
	serviceBrokers := korifiv1alpha1.CFServiceBrokerList{}
	if err := r.k8sClient.List(ctx, &serviceBrokers,
		client.InNamespace(o.GetNamespace()),
		client.MatchingFields{
			shared.IndexServiceBrokerCredentialsSecretName: o.GetName(),
		}); err != nil {
		return []reconcile.Request{}
	}

	requests := []reconcile.Request{}
	for _, sb := range serviceBrokers.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      sb.Name,
				Namespace: sb.Namespace,
			},
		})
	}

	return requests
}

//+kubebuilder:rbac:groups=korifi.cloudfoundry.org,resources=cfservicebrokers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=korifi.cloudfoundry.org,resources=cfservicebrokers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=korifi.cloudfoundry.org,resources=cfserviceofferings,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=korifi.cloudfoundry.org,resources=cfserviceplans,verbs=get;list;watch;create;update;patch

func (r *Reconciler) ReconcileResource(ctx context.Context, cfServiceBroker *korifiv1alpha1.CFServiceBroker) (ctrl.Result, error) {
	log := logr.FromContextOrDiscard(ctx).WithValues("broker-id", cfServiceBroker.Name)

	cfServiceBroker.Status.ObservedGeneration = cfServiceBroker.Generation
	log.V(1).Info("set observed generation", "generation", cfServiceBroker.Status.ObservedGeneration)

	var err error
	readyConditionBuilder := k8s.NewReadyConditionBuilder(cfServiceBroker)
	defer func() {
		meta.SetStatusCondition(&cfServiceBroker.Status.Conditions, readyConditionBuilder.WithError(err).Build())
	}()

	credentialsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: cfServiceBroker.Namespace,
			Name:      cfServiceBroker.Spec.Credentials.Name,
		},
	}
	err = r.k8sClient.Get(ctx, client.ObjectKeyFromObject(credentialsSecret), credentialsSecret)
	if err != nil {
		readyConditionBuilder.WithReason("CredentialsSecretNotAvailable")
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	if err = r.validateCredentials(credentialsSecret); err != nil {
		readyConditionBuilder.WithReason("SecretInvalid")
		return ctrl.Result{}, err
	}

	log.V(1).Info("credentials secret", "name", credentialsSecret.Name, "version", credentialsSecret.ResourceVersion)
	cfServiceBroker.Status.CredentialsObservedVersion = credentialsSecret.ResourceVersion

	catalog, err := r.catalogClient.GetCatalog(ctx, cfServiceBroker)
	if err != nil {
		log.Error(err, "failed to get catalog from broker", "broker", cfServiceBroker.Name)
		readyConditionBuilder.WithReason("GetCatalogFailed")
		return ctrl.Result{}, err
	}

	err = r.reconcileCatalog(ctx, cfServiceBroker, catalog)
	if err != nil {
		log.Error(err, "failed to reconcile catalog")
		return ctrl.Result{}, fmt.Errorf("failed to reconcile catalog: %v", err)
	}

	readyConditionBuilder.Ready()
	return ctrl.Result{}, nil
}

func (r *Reconciler) validateCredentials(credentialsSecret *corev1.Secret) error {
	creds := map[string]any{}
	err := json.Unmarshal(credentialsSecret.Data[korifiv1alpha1.CredentialsSecretKey], &creds)
	if err != nil {
		return fmt.Errorf("invalid credentials secret %q: %w", credentialsSecret.Name, err)
	}

	for _, k := range []string{korifiv1alpha1.UsernameCredentialsKey, korifiv1alpha1.PasswordCredentialsKey} {
		if _, ok := creds[k]; !ok {
			return fmt.Errorf("broker credentials secret %q does not specify %q", credentialsSecret.Name, k)
		}
	}

	return nil
}

func (r *Reconciler) reconcileCatalog(ctx context.Context, cfServiceBroker *korifiv1alpha1.CFServiceBroker, catalog *osbapi.Catalog) error {
	for _, service := range catalog.Services {
		err := r.reconcileCatalogService(ctx, cfServiceBroker, service)
		if err != nil {
			return err
		}

	}
	return nil
}

func (r *Reconciler) reconcileCatalogService(ctx context.Context, cfServiceBroker *korifiv1alpha1.CFServiceBroker, catalogService osbapi.Service) error {
	serviceOffering := &korifiv1alpha1.CFServiceOffering{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tools.NamespacedUUID(cfServiceBroker.Name, catalogService.ID),
			Namespace: cfServiceBroker.Namespace,
		},
	}

	_, err := controllerutil.CreateOrPatch(ctx, r.k8sClient, serviceOffering, func() error {
		if serviceOffering.Labels == nil {
			serviceOffering.Labels = map[string]string{}
		}
		serviceOffering.Labels[korifiv1alpha1.RelServiceBrokerLabel] = cfServiceBroker.Name

		var err error
		serviceOffering.Spec.ServiceOffering, err = toSpecServiceOffering(catalogService)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to reconcile service offering %q : %w", catalogService.ID, err)
	}

	for _, catalogPlan := range catalogService.Plans {
		err = r.reconcileCatalogPlan(ctx, serviceOffering, catalogPlan)
		if err != nil {
			return fmt.Errorf("failed to reconcile service plan %q for service offering %q: %w", catalogPlan.ID, catalogService.ID, err)
		}
	}

	return nil
}

func (r *Reconciler) reconcileCatalogPlan(ctx context.Context, serviceOffering *korifiv1alpha1.CFServiceOffering, catalogPlan osbapi.Plan) error {
	servicePlan := &korifiv1alpha1.CFServicePlan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tools.NamespacedUUID(serviceOffering.Labels[korifiv1alpha1.RelServiceBrokerLabel], catalogPlan.ID),
			Namespace: serviceOffering.Namespace,
		},
	}

	_, err := controllerutil.CreateOrPatch(ctx, r.k8sClient, servicePlan, func() error {
		if servicePlan.Labels == nil {
			servicePlan.Labels = map[string]string{}
		}
		servicePlan.Labels[korifiv1alpha1.RelServiceBrokerLabel] = serviceOffering.Labels[korifiv1alpha1.RelServiceBrokerLabel]
		servicePlan.Labels[korifiv1alpha1.RelServiceOfferingLabel] = serviceOffering.Name

		rawMetadata, err := json.Marshal(catalogPlan.Metadata)
		if err != nil {
			return fmt.Errorf("failed to marshal service plan %q metadata: %w", catalogPlan.ID, err)
		}

		servicePlan.Spec = korifiv1alpha1.CFServicePlanSpec{
			ServicePlan: services.ServicePlan{
				BrokerServicePlan: services.BrokerServicePlan{
					Name:        catalogPlan.Name,
					Free:        catalogPlan.Free,
					Description: catalogPlan.Description,
					BrokerCatalog: services.ServicePlanBrokerCatalog{
						ID: catalogPlan.ID,
						Metadata: &runtime.RawExtension{
							Raw: rawMetadata,
						},
						Features: services.ServicePlanFeatures{
							PlanUpdateable: catalogPlan.PlanUpdateable,
							Bindable:       catalogPlan.Bindable,
						},
					},
					Schemas: catalogPlan.Schemas,
				},
			},
		}

		return nil
	})

	return err
}

func toSpecServiceOffering(catalogService osbapi.Service) (services.ServiceOffering, error) {
	offering := services.ServiceOffering{
		Name:        catalogService.Name,
		Description: catalogService.Description,
		Tags:        catalogService.Tags,
		Requires:    catalogService.Requires,
		BrokerCatalog: services.ServiceBrokerCatalog{
			Id:       catalogService.ID,
			Features: catalogService.BrokerCatalogFeatures,
		},
	}

	if catalogService.Metadata != nil {
		if u, ok := catalogService.Metadata["documentationUrl"]; ok {
			offering.DocumentationURL = tools.PtrTo(u.(string))
		}

		rawMd, err := json.Marshal(catalogService.Metadata)
		if err != nil {
			return services.ServiceOffering{}, err
		}
		offering.BrokerCatalog.Metadata = &runtime.RawExtension{
			Raw: rawMd,
		}

	}

	return offering, nil
}
