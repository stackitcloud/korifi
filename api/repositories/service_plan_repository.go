package repositories

import (
	"context"
	"fmt"

	"code.cloudfoundry.org/korifi/api/authorization"
	apierrors "code.cloudfoundry.org/korifi/api/errors"
	korifiv1alpha1 "code.cloudfoundry.org/korifi/controllers/api/v1alpha1"
	"code.cloudfoundry.org/korifi/model"
	"code.cloudfoundry.org/korifi/model/services"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const ServicePlanResourceType = "Service Plan"

type ServicePlanResource struct {
	services.ServicePlan
	model.CFResource
	Relationships ServicePlanRelationships `json:"relationships"`
}

type ServicePlanRelationships struct {
	ServiceOffering model.ToOneRelationship `json:"service_offering"`
}

type ServicePlanRepo struct {
	userClientFactory authorization.UserK8sClientFactory
	rootNamespace     string
}

func NewServicePlanRepo(
	userClientFactory authorization.UserK8sClientFactory,
	rootNamespace string,
) *ServicePlanRepo {
	return &ServicePlanRepo{
		userClientFactory: userClientFactory,
		rootNamespace:     rootNamespace,
	}
}

func (r *ServicePlanRepo) ListPlans(ctx context.Context, authInfo authorization.Info) ([]ServicePlanResource, error) {
	userClient, err := r.userClientFactory.BuildClient(authInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to build user client: %w", err)
	}

	cfServicePlans := &korifiv1alpha1.CFServicePlanList{}
	if err := userClient.List(ctx, cfServicePlans, client.InNamespace(r.rootNamespace)); err != nil {
		return nil, apierrors.FromK8sError(err, ServicePlanResourceType)
	}

	var result []ServicePlanResource
	for _, plan := range cfServicePlans.Items {
		result = append(result, ServicePlanResource{
			ServicePlan: plan.Spec.ServicePlan,
			CFResource: model.CFResource{
				GUID:      plan.Name,
				CreatedAt: plan.CreationTimestamp.Time,
				Metadata: model.Metadata{
					Labels:      plan.Labels,
					Annotations: plan.Annotations,
				},
			},
			Relationships: ServicePlanRelationships{
				ServiceOffering: model.ToOneRelationship{
					Data: model.Relationship{
						GUID: plan.Labels[korifiv1alpha1.RelServiceOfferingLabel],
					},
				},
			},
		})
	}

	return result, nil
}
