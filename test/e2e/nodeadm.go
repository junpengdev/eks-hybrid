//go:build e2e
// +build e2e

package e2e

import (
	"context"
	_ "embed"
	"fmt"
	"math/rand"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/aws/eks-hybrid/internal/api"
	"github.com/aws/eks-hybrid/internal/creds"
	"github.com/go-logr/logr"
	"github.com/tredoe/osutil/user/crypt"
	"github.com/tredoe/osutil/user/crypt/sha512_crypt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const ssmActivationName = "eks-hybrid-ssm-provider"
const amd64Arch = "x86_64"
const arm64Arch = "arm64"

type UserDataInput struct {
	CredsProviderName string
	KubernetesVersion string
	NodeadmUrls       NodeadmURLs
	NodeadmConfigYaml string
	Provider          string
	RootPasswordHash  string
}

// NodeadmOS defines an interface for operating system-specific behavior.
type NodeadmOS interface {
	Name() string
	AMIName(ctx context.Context, awsSession *session.Session) (string, error)
	BuildUserData(UserDataInput UserDataInput) ([]byte, error)
	InstanceType() string
}

type NodeadmCredentialsProvider interface {
	Name() creds.CredentialProvider
	NodeadmConfig(cluster *hybridCluster) (*api.NodeConfig, error)
}

type SsmProvider struct {
	ssmClient *ssm.SSM
	role      string
}

type NodeadmURLs struct {
	AMD string
	ARM string
}

func (s *SsmProvider) Name() creds.CredentialProvider {
	return creds.SsmCredentialProvider
}

func (s *SsmProvider) NodeadmConfig(cluster *hybridCluster) (*api.NodeConfig, error) {
	ssmActivationDetails, err := createSSMActivation(s.ssmClient, s.role, ssmActivationName)
	if err != nil {
		return nil, err
	}
	return &api.NodeConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "node.eks.aws/v1alpha1",
			Kind:       "NodeConfig",
		},
		Spec: api.NodeConfigSpec{
			Cluster: api.ClusterDetails{
				Name:   cluster.clusterName,
				Region: cluster.clusterRegion,
			},
			Hybrid: &api.HybridOptions{
				SSM: &api.SSM{
					ActivationID:   *ssmActivationDetails.ActivationId,
					ActivationCode: *ssmActivationDetails.ActivationCode,
				},
			},
		},
	}, nil
}

func parseS3URL(s3URL string) (bucket, key string, err error) {
	parsedURL, err := url.Parse(s3URL)
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(parsedURL.Host, ".", 2)
	bucket = parts[0]
	key = strings.TrimPrefix(parsedURL.Path, "/")
	return bucket, key, nil
}

func generatePreSignedURL(client *s3.S3, bucket, key string, expiration time.Duration) (string, error) {
	req, _ := client.GetObjectRequest(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})

	url, err := req.Presign(expiration)
	if err != nil {
		return "", fmt.Errorf("generating pre-signed URL: %v", err)
	}
	return url, nil
}

func getNodeadmURL(client *s3.S3, nodeadmUrl string) (string, error) {
	s3Bucket, s3BucketKey, err := parseS3URL(nodeadmUrl)
	if err != nil {
		return "", fmt.Errorf("parsing S3 URL: %v", err)
	}

	preSignedURL, err := generatePreSignedURL(client, s3Bucket, s3BucketKey, 30*time.Minute)
	if err != nil {
		return "", fmt.Errorf("getting presigned URL for nodeadm: %v", err)
	}
	return preSignedURL, nil
}

func runNodeadmUninstall(ctx context.Context, client *ssm.SSM, instanceID string, logger logr.Logger) error {
	commands := []string{
		// TODO: @pjshah run uninstall without node-validation and pod-validation flags after adding cordon and drain node functionality
		"set -eux",
		"sudo /tmp/nodeadm uninstall -skip node-validation,pod-validation",
		"sudo cloud-init clean --logs",
		"sudo rm -rf /var/lib/cloud/instances",
	}
	ssmConfig := &ssmConfig{
		client:     client,
		instanceID: instanceID,
		commands:   commands,
	}
	outputs, err := ssmConfig.runCommandsOnInstanceWaitForInProgress(ctx, logger)
	if err != nil {
		return fmt.Errorf("running SSM command: %w", err)
	}
	logger.Info("Nodeadm Uninstall", "output", outputs)
	for _, output := range outputs {
		if *output.Status != "InProgress" {
			return fmt.Errorf("node uninstall SSM command did not properly reach InProgress")
		}
	}
	return nil
}

func generateOSPassword() (string, string, error) {
	// Generate a random string for use in the salt
	const letters = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const length = 8
	password := make([]byte, length)
	for i := range password {
		password[i] = letters[rand.Intn(len(letters))]
	}
	c := crypt.New(crypt.SHA512)
	s := sha512_crypt.GetSalt()
	salt := s.GenerateWRounds(s.SaltLenMax, 4096)
	hash, err := c.Generate(password, salt)
	if err != nil {
		return "", "", fmt.Errorf("error gemerating root password: %s\n", err)
	}
	return string(password), string(hash), nil
}