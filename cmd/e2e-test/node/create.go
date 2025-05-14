package node

import (
	"context"
	"fmt"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	ssmsdk "github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/go-logr/logr"
	"github.com/integrii/flaggy"
	"go.uber.org/zap"
	"k8s.io/client-go/dynamic"
	clientgo "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/aws/eks-hybrid/internal/cli"
	"github.com/aws/eks-hybrid/test/e2e"
	"github.com/aws/eks-hybrid/test/e2e/cluster"
	"github.com/aws/eks-hybrid/test/e2e/credentials"
	"github.com/aws/eks-hybrid/test/e2e/kubernetes"
	osystem "github.com/aws/eks-hybrid/test/e2e/os"
	"github.com/aws/eks-hybrid/test/e2e/peered"
	"github.com/aws/eks-hybrid/test/e2e/s3"
)

type create struct {
	flaggy        *flaggy.Subcommand
	configFile    string
	instanceName  string
	instanceSize  string
	instanceType  string
	credsProvider string
	os            string
	arch          string
}

func NewCreateCommand() cli.Command {
	cmd := create{
		os:           "al23",
		arch:         "amd64",
		instanceSize: "Large",
	}

	createCmd := flaggy.NewSubcommand("create")
	createCmd.Description = "Create a Hybrid Node"
	createCmd.AddPositionalValue(&cmd.instanceName, "INSTANCE_NAME", 1, true, "Name of the instance to create.")
	createCmd.String(&cmd.configFile, "f", "config-file", "Path tests config file.")
	createCmd.String(&cmd.credsProvider, "c", "creds-provider", "Credentials provider to use (iam-ra, ssm).")
	createCmd.String(&cmd.os, "o", "os", "OS to use (al23, ubuntu2004, ubuntu2204, ubuntu2404, rhel8, rhel9).")
	createCmd.String(&cmd.arch, "a", "arch", "Architecture to use (amd64, arm64).")
	createCmd.String(&cmd.instanceSize, "s", "instance-size", "Instance size to use (Large, XLarge).")
	createCmd.String(&cmd.instanceType, "t", "instance-type", "Instance type to use (t3.large, g4dn.xlarge, etc). If provided, instance size would be ignored.")

	cmd.flaggy = createCmd

	return &cmd
}

//TODO: remove later, provide node create command to other packages. 
func NewDefaultCreateCommnad(name, configFile string) cli.Command {
	cmd := create{
		os:            "al23",
		arch:          "amd64",
		instanceSize:  "Large",
		credsProvider: "ssm",
		instanceName:  name,
		configFile:    configFile,
	}

	return &cmd
}

func (c *create) Flaggy() *flaggy.Subcommand {
	return c.flaggy
}

func (c *create) Run(log *zap.Logger, opts *cli.GlobalOptions) error {
	ctx := context.Background()
	config, err := e2e.ReadConfig(c.configFile)
	if err != nil {
		return err
	}

	logger := e2e.NewLogger()
	aws, err := e2e.NewAWSConfig(ctx, awsconfig.WithRegion(config.ClusterRegion))
	if err != nil {
		return fmt.Errorf("reading AWS configuration: %w", err)
	}

	infra, err := peered.Setup(ctx, logger, aws, config.ClusterName, config.Endpoint)
	if err != nil {
		return fmt.Errorf("setting up test infrastructure: %w", err)
	}

	eksClient := e2e.NewEKSClient(aws, config.Endpoint)
	ec2Client := ec2.NewFromConfig(aws)
	ssmClient := ssmsdk.NewFromConfig(aws)
	s3Client := s3sdk.NewFromConfig(aws)

	clientConfig, err := clientcmd.BuildConfigFromFlags("", cluster.KubeconfigPath(config.ClusterName))
	if err != nil {
		return err
	}
	k8s, err := clientgo.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	k8sDynamic, err := dynamic.NewForConfig(clientConfig)
	if err != nil {
		return err
	}

	cluster, err := peered.GetHybridCluster(ctx, eksClient, ec2Client, config.ClusterName)
	if err != nil {
		return err
	}

	urls, err := s3.BuildNodeamURLs(ctx, s3Client, config.NodeadmUrlAMD, config.NodeadmUrlARM)
	if err != nil {
		return err
	}

	node := peered.NodeCreate{
		AWS:     aws,
		Cluster: cluster,
		EC2:     ec2Client,
		SSM:     ssmClient,
		Logger:  logger,

		NodeadmURLs: *urls,
		PublicKey:   infra.NodesPublicSSHKey,
	}

	nodeOS, err := buildOS(c.os, c.arch)
	if err != nil {
		return err
	}

	var credsProvider e2e.NodeadmCredentialsProvider

	switch c.credsProvider {
	case "iam-ra":
		credsProvider = &credentials.IamRolesAnywhereProvider{
			RoleARN:        infra.Credentials.IRANodeRoleARN,
			ProfileARN:     infra.Credentials.IRAProfileARN,
			TrustAnchorARN: infra.Credentials.IRATrustAnchorARN,
			CA:             infra.Credentials.RolesAnywhereCA,
		}
	case "ssm":
		credsProvider = &credentials.SsmProvider{
			SSM:  ssmClient,
			Role: infra.Credentials.SSMNodeRoleName,
		}
	}

	instanceSize := e2e.Large
	if c.instanceSize == "XLarge" {
		instanceSize = e2e.XLarge
	}

	peerdNode, err := node.Create(ctx, &peered.NodeSpec{
		InstanceName:   c.instanceName,
		InstanceSize:   instanceSize,
		InstanceType:   c.instanceType,
		NodeK8sVersion: cluster.KubernetesVersion,
		NodeName:       c.instanceName,
		OS:             nodeOS,
		Provider:       credsProvider,
	})
	if err != nil {
		return err
	}

	logger.Info("Node created", "instanceID", peerdNode.Instance.ID)

	logger.Info("Connecting to the node serial console...")
	serial, err := node.SerialConsole(ctx, peerdNode.Instance.ID)
	if err != nil {
		return fmt.Errorf("preparing EC2 for serial connection: %w", err)
	}
	defer serial.Close()

	pausableOutput := e2e.NewSwitchWriter(os.Stdout)
	pausableOutput.Pause()
	if err := serial.Copy(pausableOutput); err != nil {
		return fmt.Errorf("connecting to EC2 serial console: %w", err)
	}

	logger.Info("Waiting for the node to initialize...")
	if err := pausableOutput.Resume(); err != nil {
		return fmt.Errorf("resuming output: %w", err)
	}

	verifyNode := kubernetes.VerifyNode{
		K8s:      k8s,
		Logger:   logr.Discard(),
		NodeName: peerdNode.Name,
		NodeIP:   peerdNode.Instance.IP,
	}
	vn, err := verifyNode.WaitForNodeReady(ctx)
	if err != nil {
		return fmt.Errorf("waiting for node to be ready: %w", err)
	}
	pausableOutput.Pause()
	fmt.Println() // newline after pausing the serial output to ensure a clean log after
	logger.Info("Node is ready", "nodeName", vn.Name)

	network := peered.Network{
		EC2:    ec2Client,
		Logger: logger,
		K8s: peered.K8s{
			Interface: k8s,
			Dynamic:   k8sDynamic,
		},
		Cluster: cluster,
	}

	if err := network.CreateRoutesForNode(ctx, &peerdNode); err != nil {
		return fmt.Errorf("creating routes for node: %w", err)
	}

	return nil
}

func buildOS(osName, arch string) (e2e.NodeadmOS, error) {
	osBuilders, ok := oses[osName]
	if !ok {
		return nil, fmt.Errorf("unknown OS %s", osName)
	}

	build, ok := osBuilders[arch]
	if !ok {
		return nil, fmt.Errorf("unknown architecture %q for OS %q", arch, osName)
	}

	return build(), nil
}

var oses = map[string]map[string]func() e2e.NodeadmOS{
	"al23": {
		"amd64": func() e2e.NodeadmOS { return osystem.NewAmazonLinux2023AMD() },
		"arm64": func() e2e.NodeadmOS { return osystem.NewAmazonLinux2023ARM() },
	},
	"ubuntu2004": {
		"amd64": func() e2e.NodeadmOS { return osystem.NewUbuntu2004AMD() },
		"arm64": func() e2e.NodeadmOS { return osystem.NewUbuntu2004ARM() },
	},
	"ubuntu2204": {
		"amd64": func() e2e.NodeadmOS { return osystem.NewUbuntu2204AMD() },
		"arm64": func() e2e.NodeadmOS { return osystem.NewUbuntu2204ARM() },
	},
	"ubuntu2404": {
		"amd64": func() e2e.NodeadmOS { return osystem.NewUbuntu2404AMD() },
		"arm64": func() e2e.NodeadmOS { return osystem.NewUbuntu2404ARM() },
	},
	"rhel8": {
		"amd64": func() e2e.NodeadmOS {
			return osystem.NewRedHat8AMD(os.Getenv("RHEL_USERNAME"), os.Getenv("RHEL_PASSWORD"))
		},
		"arm64": func() e2e.NodeadmOS {
			return osystem.NewRedHat8ARM(os.Getenv("RHEL_USERNAME"), os.Getenv("RHEL_PASSWORD"))
		},
	},
	"rhel9": {
		"amd64": func() e2e.NodeadmOS {
			return osystem.NewRedHat9AMD(os.Getenv("RHEL_USERNAME"), os.Getenv("RHEL_PASSWORD"))
		},
		"arm64": func() e2e.NodeadmOS {
			return osystem.NewRedHat9ARM(os.Getenv("RHEL_USERNAME"), os.Getenv("RHEL_PASSWORD"))
		},
	},
}
