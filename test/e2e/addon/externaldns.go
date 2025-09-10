package addon

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/go-logr/logr"
	"k8s.io/client-go/rest"

	e2errors "github.com/aws/eks-hybrid/test/e2e/errors"

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
	HostedZoneId       string
	PodIdentityRoleArn string
}

// Create installs the external-dns addon
func (e *ExternalDNSTest) Create(ctx context.Context) error {
	hostedZoneId, err := e.getHostedZoneId(ctx)
	if err != nil {
		return fmt.Errorf("failed to get hosted zone id: %w", err)
	}

	e.HostedZoneId = *hostedZoneId
	hostedZoneName, err := e.getHostedZoneName(ctx, hostedZoneId)
	if err != nil {
		return fmt.Errorf("failed to get hosted zone name: %w", err)
	}

	e.Logger.Info("Hosted zone", "Id", hostedZoneId, "Name", hostedZoneName)

	configuration := fmt.Sprintf("{\"domainFilters\": [%s], \"policy\": \"sync\"}", *hostedZoneName)
	e.Logger.Info("external-dns configuration", "configuration", configuration)
	e.addon = &Addon{
		Cluster:       e.Cluster,
		Namespace:     externalDNSNamespace,
		Name:          externalDNS,
		Configuration: configuration,
	}

	// Create pod identity association for the addon's service account
	if err := e.setupPodIdentity(ctx); err != nil {
		return fmt.Errorf("failed to setup Pod Identity for external-dns: %v", err)
	}

	if err := e.addon.CreateAndWaitForActive(ctx, e.EKSClient, e.K8S, e.Logger); err != nil {
		return err
	}

	// TODO: remove the following call once the addon is updated to work with hybrid nodes
	// Remove anti affinity to allow external-dns to be deployed to hybrid nodes
	if err := kubernetes.RemoveDeploymentAntiAffinity(ctx, e.K8S, externalDNSDeployment, externalDNSNamespace, e.Logger); err != nil {
		return fmt.Errorf("failed to remove anti affinity: %w", err)
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

func (e *ExternalDNSTest) setupPodIdentity(ctx context.Context) error {
	e.Logger.Info("Setting up Pod Identity for external-dns")

	// Create Pod Identity Association for the addon's service account
	createAssociationInput := &eks.CreatePodIdentityAssociationInput{
		ClusterName:    aws.String(e.Cluster),
		Namespace:      aws.String(externalDNSNamespace),
		RoleArn:        aws.String(e.PodIdentityRoleArn),
		ServiceAccount: aws.String(externalDNSServiceAccount),
	}

	createAssociationOutput, err := e.EKSClient.CreatePodIdentityAssociation(ctx, createAssociationInput)

	if err != nil && e2errors.IsType(err, &ekstypes.ResourceInUseException{}) {
		e.Logger.Info("Pod Identity Association already exists for service account", "serviceAccount", externalDNSServiceAccount)
		return nil
	}

	if err != nil {
		return fmt.Errorf("failed to create Pod Identity Association: %v", err)
	}

	e.Logger.Info("Created Pod Identity Association", "associationID", *createAssociationOutput.Association.AssociationId)
	return nil
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

func (e *ExternalDNSTest) getHostedZoneName(ctx context.Context, hostedZoneId *string) (*string, error) {
	zoneOutput, err := e.Route53Client.GetHostedZone(ctx, &route53.GetHostedZoneInput{
		Id: hostedZoneId,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get hosted zone: %w", err)
	}

	return zoneOutput.HostedZone.Name, nil
}
