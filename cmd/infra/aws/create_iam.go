package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/iam/iamiface"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3iface"
	"github.com/spf13/cobra"

	hyperv1 "github.com/openshift/hypershift/api/v1alpha1"
	awsutil "github.com/openshift/hypershift/cmd/infra/aws/util"
)

type CreateIAMOptions struct {
	Region             string
	AWSCredentialsFile string
	InfraID            string
	OutputFile         string

	IAMClient iamiface.IAMAPI
	S3Client  s3iface.S3API
}

type CreateIAMOutput struct {
	Region                   string                       `json:"region"`
	ProfileName              string                       `json:"profileName"`
	InfraID                  string                       `json:"infraID"`
	IssuerURL                string                       `json:"issuerURL"`
	ServiceAccountSigningKey []byte                       `json:"serviceAccountSigningKey"`
	Roles                    []hyperv1.AWSRoleCredentials `json:"roles"`

	KubeCloudControllerUserAccessKeyID     string `json:"kubeCloudControllerUserAccessKeyID"`
	KubeCloudControllerUserAccessKeySecret string `json:"kubeCloudControllerUserAccessKeySecret"`
	NodePoolManagementUserAccessKeyID      string `json:"nodePoolManagementUserAccessKeyID"`
	NodePoolManagementUserAccessKeySecret  string `json:"nodePoolManagementUserAccessKeySecret"`
}

func NewCreateIAMCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:          "aws",
		Short:        "Creates AWS instance profile for workers",
		SilenceUsage: true,
	}

	opts := CreateIAMOptions{
		Region:             "us-east-1",
		AWSCredentialsFile: "",
		InfraID:            "",
	}

	cmd.Flags().StringVar(&opts.AWSCredentialsFile, "aws-creds", opts.AWSCredentialsFile, "Path to an AWS credentials file (required)")
	cmd.Flags().StringVar(&opts.InfraID, "infra-id", opts.InfraID, "Infrastructure ID to use for AWS resources.")
	cmd.Flags().StringVar(&opts.Region, "region", opts.Region, "Region where cluster infra should be created")
	cmd.Flags().StringVar(&opts.OutputFile, "output-file", opts.OutputFile, "Path to file that will contain output information from infra resources (optional)")

	cmd.MarkFlagRequired("aws-creds")
	cmd.MarkFlagRequired("infra-id")

	cmd.Run = func(cmd *cobra.Command, args []string) {
		ctx, cancel := context.WithCancel(context.Background())
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT)
		go func() {
			<-sigs
			cancel()
		}()

		awsSession := awsutil.NewSession()
		awsConfig := awsutil.NewConfig(opts.AWSCredentialsFile, opts.Region)
		opts.IAMClient = iam.New(awsSession, awsConfig)
		opts.S3Client = s3.New(awsSession, awsConfig)

		if err := opts.Run(ctx); err != nil {
			log.Error(err, "Failed to create infrastructure")
			os.Exit(1)
		}
	}

	return cmd
}

func (o *CreateIAMOptions) Run(ctx context.Context) error {
	results, err := o.CreateIAM(ctx)
	if err != nil {
		return err
	}
	// Write out stateful information
	out := os.Stdout
	if len(o.OutputFile) > 0 {
		var err error
		out, err = os.Create(o.OutputFile)
		if err != nil {
			return fmt.Errorf("cannot create output file: %w", err)
		}
		defer out.Close()
	}
	outputBytes, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize result: %w", err)
	}
	_, err = out.Write(outputBytes)
	if err != nil {
		return fmt.Errorf("failed to write result: %w", err)
	}
	return nil
}

func (o *CreateIAMOptions) CreateIAM(ctx context.Context) (*CreateIAMOutput, error) {
	var err error

	results, err := o.CreateOIDCResources(o.IAMClient, o.S3Client)
	if err != nil {
		return nil, err
	}
	profileName := DefaultProfileName(o.InfraID)
	results.ProfileName = profileName
	err = o.CreateWorkerInstanceProfile(o.IAMClient, profileName)
	if err != nil {
		return nil, err
	}
	log.Info("Created IAM profile", "name", profileName, "region", o.Region)

	if key, err := o.CreateCredentialedUserWithPolicy(ctx, o.IAMClient, fmt.Sprintf("%s-%s", o.InfraID, "cloud-controller"), cloudControllerPolicy); err != nil {
		return nil, err
	} else {
		results.KubeCloudControllerUserAccessKeyID = aws.StringValue(key.AccessKeyId)
		results.KubeCloudControllerUserAccessKeySecret = aws.StringValue(key.SecretAccessKey)
	}

	if key, err := o.CreateCredentialedUserWithPolicy(ctx, o.IAMClient, fmt.Sprintf("%s-%s", o.InfraID, "node-pool"), nodePoolPolicy); err != nil {
		return nil, err
	} else {
		results.NodePoolManagementUserAccessKeyID = aws.StringValue(key.AccessKeyId)
		results.NodePoolManagementUserAccessKeySecret = aws.StringValue(key.SecretAccessKey)
	}

	return results, nil
}
