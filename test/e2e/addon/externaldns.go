package addon

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/eks"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"

	"github.com/aws/eks-hybrid/test/e2e/constants"
	"github.com/aws/eks-hybrid/test/e2e/kubernetes"
	peeredtypes "github.com/aws/eks-hybrid/test/e2e/peered/types"
)

const (
	externalDNS               = "external-dns"
	externalDNSNamespace      = "external-dns"
	externalDNSDeployment     = "external-dns"
	externalDNSServiceAccount = "external-dns"
	externalDNSWaitTimeout    = 5 * time.Minute
)

// ExternalDNSTest tests the external-dns addon
type ExternalDNSTest struct {
	Cluster            string
	addon              *Addon
	K8S                peeredtypes.K8s
	EKSClient          *eks.Client
	Route53Client      *route53.Client
	K8SConfig          *rest.Config
	Logger             logr.Logger
	PodIdentityRoleArn string
}

// Create installs the external-dns addon
func (e *ExternalDNSTest) Create(ctx context.Context) error {
	hostedZoneId, err := e.getHostedZoneId(ctx)
	if err != nil {
		return fmt.Errorf("failed to get hosted zone id: %w", err)
	}

	e.Logger.Info("Hosted zone", "Id", hostedZoneId)

	e.addon = &Addon{
		Cluster:   e.Cluster,
		Namespace: externalDNSNamespace,
		Name:      externalDNS,
		PodIdentityAssociations: []PodIdentityAssociation{
			{
				RoleArn:        e.PodIdentityRoleArn,
				ServiceAccount: externalDNSServiceAccount,
			},
		},
	}

	if err := e.addon.CreateAndWaitForActive(ctx, e.EKSClient, e.K8S, e.Logger); err != nil {
		return err
	}

	// Wait for external-dns deployment to be ready
	if err := kubernetes.DeploymentWaitForReplicas(ctx, externalDNSWaitTimeout, e.K8S, externalDNSNamespace, externalDNSDeployment); err != nil {
		return fmt.Errorf("deployment %s not ready: %w", externalDNSDeployment, err)
	}

	return nil
}

// Validate checks if external-dns is working correctly
func (e *ExternalDNSTest) Validate(ctx context.Context) error {
	// TODO: add validate later
	return nil
}

func (e *ExternalDNSTest) Delete(ctx context.Context) error {
	return e.addon.Delete(ctx, e.EKSClient, e.Logger)
}


func (e *ExternalDNSTest) getHostedZoneId(ctx context.Context) (*string, error) {
	output, err := e.Route53Client.ListHostedZones(ctx, &route53.ListHostedZonesInput{
		HostedZoneType: types.HostedZoneTypePrivateHostedZone,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list hosted zones: %w", err)
	}

	var hostedZoneIds []string

	for _, hostedZone := range output.HostedZones {
		hostedZoneIds = append(hostedZoneIds, strings.Split(*hostedZone.Id, "/")[2])
	}

	listTagsOutput, err := e.Route53Client.ListTagsForResources(ctx, &route53.ListTagsForResourcesInput{
		ResourceIds:  hostedZoneIds,
		ResourceType: types.TagResourceTypeHostedzone,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list tags for hosted zones: %w", err)
	}

	for _, resourceTagSet := range listTagsOutput.ResourceTagSets {
		for _, tag := range resourceTagSet.Tags {
			if *tag.Key == constants.TestClusterTagKey && *tag.Value == e.Cluster {
				return resourceTagSet.ResourceId, nil
			}
		}
	}

	return nil, fmt.Errorf("hosted zone not found for cluster %s", e.Cluster)
}
