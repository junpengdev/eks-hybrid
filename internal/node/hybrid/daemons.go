package hybrid

import (
	"github.com/pkg/errors"

	"github.com/aws/eks-hybrid/internal/containerd"
	"github.com/aws/eks-hybrid/internal/daemon"
	"github.com/aws/eks-hybrid/internal/kubelet"
	"github.com/aws/eks-hybrid/internal/ssm"
)

func (hnp *hybridNodeProvider) withDaemonManager() error {
	manager, err := daemon.NewDaemonManager()
	if err != nil {
		return err
	}
	hnp.daemonManager = manager
	return nil
}

func (hnp *hybridNodeProvider) GetDaemons() ([]daemon.Daemon, error) {
	if hnp.awsConfig == nil {
		return nil, errors.New("aws config not set")
	}
	return []daemon.Daemon{
		containerd.NewContainerdDaemon(hnp.daemonManager, hnp.nodeConfig, hnp.awsConfig),
		kubelet.NewKubeletDaemon(hnp.daemonManager, hnp.nodeConfig, hnp.awsConfig),
	}, nil
}

func (hnp *hybridNodeProvider) PreProcessDaemon() error {
	if hnp.nodeConfig.IsSSM() {
		ssmDaemon := ssm.NewSsmDaemon(hnp.daemonManager, hnp.nodeConfig, hnp.logger)
		if err := ssmDaemon.Configure(); err != nil {
			return err
		}
		if err := ssmDaemon.EnsureRunning(); err != nil {
			return err
		}
	}
	return nil
}