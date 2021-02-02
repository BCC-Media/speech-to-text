package main

import (
	"fmt"
	"os"

	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/cloudfunctions"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v2/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v2/go/pulumi/config"
)

var pulumiServiceAccount = os.Getenv("PULUMI_GOOGLE_ACCOUT")
var billingAccountID = os.Getenv("BILLING_ACCOUNT_ID")

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		googleCfg := config.New(ctx, "gcp")

		gcpProjectID := googleCfg.Require("project")

		project, err := organizations.NewProject(ctx, gcpProjectID, &organizations.ProjectArgs{
			BillingAccount: pulumi.StringPtr(billingAccountID),
			ProjectId:      pulumi.String(gcpProjectID),
		})
		if err != nil {
			return err
		}

		iamAPI, err := projects.NewService(ctx, "iamAPI", &projects.ServiceArgs{
			DisableDependentServices: pulumi.Bool(true),
			Project:                  pulumi.String(gcpProjectID),
			Service:                  pulumi.String("iam.googleapis.com"),
		})

		mediaBankServiceAccount, err := serviceaccount.NewAccount(ctx, "mediaBankServiceAccount", &serviceaccount.AccountArgs{
			AccountId:   pulumi.String("mediabank"),
			DisplayName: pulumi.String("Media Bank"),
			Project:     pulumi.String(gcpProjectID),
		},
			pulumi.DependsOn([]pulumi.Resource{project, iamAPI}),
		)

		if err != nil {
			return err
		}

		mediaBankKey, err := serviceaccount.NewKey(ctx, "mediaBankKey", &serviceaccount.KeyArgs{
			ServiceAccountId: mediaBankServiceAccount.AccountId,
		}, pulumi.DependsOn([]pulumi.Resource{project}))
		if err != nil {
			return err
		}

		cfAPI, err := projects.NewService(ctx, "project", &projects.ServiceArgs{
			DisableDependentServices: pulumi.Bool(true),
			Service:                  pulumi.String("cloudfunctions.googleapis.com"),
		}, pulumi.DependsOn([]pulumi.Resource{project}))

		codeBucket, err := storage.NewBucket(ctx, fmt.Sprintf("%s-code", gcpProjectID), &storage.BucketArgs{
			Location:                 pulumi.String("EUROPE-WEST3"),
			UniformBucketLevelAccess: pulumi.BoolPtr(false),
		}, pulumi.DependsOn([]pulumi.Resource{project}))
		if err != nil {
			return err
		}

		// Create an object in our bucket with our function.
		bucketObjectArgs := &storage.BucketObjectArgs{
			Bucket: codeBucket.Name,
			Source: pulumi.NewFileArchive("../ingest-func"),
			Name:   pulumi.StringPtr("ingest-func.zip"),
		}

		bucketObject, err := storage.NewBucketObject(ctx, "ingest", bucketObjectArgs)
		if err != nil {
			return err
		}

		// Set arguments for creating the function resource.
		args := &cloudfunctions.FunctionArgs{
			SourceArchiveBucket: codeBucket.Name,
			Runtime:             pulumi.String("go113"),
			SourceArchiveObject: bucketObject.Name,
			EntryPoint:          pulumi.String("Ingest"),
			TriggerHttp:         pulumi.Bool(true),
			AvailableMemoryMb:   pulumi.Int(128),
			Project:             pulumi.String(gcpProjectID),
		}

		// Create the function using the args.
		ingestFunc, err := cloudfunctions.NewFunction(ctx, "ingest", args, pulumi.DependsOn(
			[]pulumi.Resource{
				project,
				cfAPI,
			},
		))
		if err != nil {
			return err
		}

		// Create a GCP resource (Storage Bucket)
		ingestBucket, err := storage.NewBucket(ctx, fmt.Sprintf("%s-ingest", gcpProjectID), &storage.BucketArgs{
			Location:                 pulumi.String("EUROPE-WEST3"),
			Project:                  pulumi.String(gcpProjectID),
			UniformBucketLevelAccess: pulumi.BoolPtr(false),
		}, pulumi.DependsOn([]pulumi.Resource{project}))
		if err != nil {
			return err
		}

		outputBucket, err := storage.NewBucket(ctx, fmt.Sprintf("%s-output", gcpProjectID), &storage.BucketArgs{
			Location:                 pulumi.String("EUROPE-WEST3"),
			Project:                  pulumi.String(gcpProjectID),
			UniformBucketLevelAccess: pulumi.BoolPtr(false),
		}, pulumi.DependsOn([]pulumi.Resource{project}))
		if err != nil {
			return err
		}

		bucketPerms := pulumi.StringArray{
			pulumi.Sprintf("OWNER:user-%s", pulumiServiceAccount),
			pulumi.Sprintf("READER:user-%s@appspot.gserviceaccount.com", project.ProjectId),
			pulumi.Sprintf("WRITER:user-%s@appspot.gserviceaccount.com", project.ProjectId),
			pulumi.Sprintf("READER:user-%s", mediaBankServiceAccount.Email),
			pulumi.Sprintf("WRITER:user-%s", mediaBankServiceAccount.Email),
		}

		_, err = storage.NewBucketACL(ctx, "ingest_store_acl", &storage.BucketACLArgs{
			Bucket:       ingestBucket.Name,
			RoleEntities: bucketPerms,
		})

		_, err = storage.NewBucketACL(ctx, "output_store_acl", &storage.BucketACLArgs{
			Bucket:       ingestBucket.Name,
			RoleEntities: bucketPerms,
		})

		// Export the DNS name of the bucket
		ctx.Export("ingestBucket", ingestBucket.Url)
		ctx.Export("outputBucket", outputBucket.Url)
		ctx.Export("ingestTrigger", ingestFunc.HttpsTriggerUrl)
		ctx.Export("mediabankKey", mediaBankKey.PrivateKey)
		return nil
	})
}
