package imagecredentialprovider

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/aws/eks-hybrid/internal/artifact"
	"github.com/aws/eks-hybrid/internal/tracker"
)

const (
	// BinPath is the path to the image-credential-provider binary.
	BinPath = "/etc/eks/image-credential-provider/ecr-credential-provider"

	artifactName      = "image-credential-provider"
	artifactFilePerms = 0o755
)

// Source represents a source that serves an image-credential-provider binary.
type Source interface {
	GetImageCredentialProvider(context.Context) (artifact.Source, error)
}

// InstallOptions contains options for installing image credential provider
type InstallOptions struct {
	InstallRoot string
	Tracker     *tracker.Tracker
	Source      Source
	Logger      *zap.Logger
}

// Install installs the image-credential-provider at BinPath.
func Install(ctx context.Context, opts InstallOptions) error {
	if err := installFromSource(ctx, opts); err != nil {
		return errors.Wrap(err, "installing image-credential-provider")
	}

	if err := opts.Tracker.Add(artifact.ImageCredentialProvider); err != nil {
		return errors.Wrap(err, "adding image-credential-provider to tracker")
	}

	return nil
}

func installFromSource(ctx context.Context, opts InstallOptions) error {
	if err := downloadFileWithRetries(ctx, opts); err != nil {
		return errors.Wrap(err, "downloading image-credential-provider")
	}

	return nil
}

func downloadFileWithRetries(ctx context.Context, opts InstallOptions) error {
	// Retry up to 3 times to download and validate the checksum
	var err error
	for range 3 {
		err = downloadFileTo(ctx, opts)
		if err == nil {
			break
		}
		opts.Logger.Error("Downloading image-credential-provider failed. Retrying...", zap.Error(err))
	}
	return err
}

func downloadFileTo(ctx context.Context, opts InstallOptions) error {
	imageCredentialProvider, err := opts.Source.GetImageCredentialProvider(ctx)
	if err != nil {
		return errors.Wrap(err, "getting image-credential-provider source")
	}
	defer imageCredentialProvider.Close()

	if err := artifact.InstallFile(filepath.Join(opts.InstallRoot, BinPath), imageCredentialProvider, artifactFilePerms); err != nil {
		return errors.Wrap(err, "installing image-credential-provider")
	}

	if !imageCredentialProvider.VerifyChecksum() {
		return errors.Errorf("image-credential-provider checksum mismatch: %v", artifact.NewChecksumError(imageCredentialProvider))
	}

	return nil
}

func Uninstall() error {
	return os.RemoveAll(path.Dir(BinPath))
}

func Upgrade(ctx context.Context, src Source, log *zap.Logger) error {
	imageCredentialProvider, err := src.GetImageCredentialProvider(ctx)
	if err != nil {
		return errors.Wrap(err, "getting image-credential-provider source")
	}
	defer imageCredentialProvider.Close()

	return artifact.Upgrade(artifactName, BinPath, imageCredentialProvider, artifactFilePerms, log)
}
