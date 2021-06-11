package main

import (
	"fmt"
	"net/url"
	"os"

	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/cloudfunctions"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/cloudscheduler"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/organizations"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/projects"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/serviceaccount"
	"github.com/pulumi/pulumi-gcp/sdk/v4/go/gcp/storage"
	"github.com/pulumi/pulumi/sdk/v2/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v2/go/pulumi/config"
)

var pulumiServiceAccount = os.Getenv("PULUMI_GOOGLE_ACCOUT")
var billingAccountID = os.Getenv("BILLING_ACCOUNT_ID")
var functionKey = os.Getenv("FUNCTION_KEY")
var cleanBucketAfterDays = 7

func appendFunctionKey(s string) string {
	parsed, err := url.Parse(s)
	if err != nil {
		return s
	}
	q := parsed.Query()
	q.Add("key", functionKey)
	parsed.RawQuery = q.Encode()
	return parsed.String()
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		googleCfg := config.New(ctx, "gcp")

		gcpProjectID := googleCfg.Require("project")

		project, err := organizations.NewProject(ctx, gcpProjectID, &organizations.ProjectArgs{
			BillingAccount: pulumi.StringPtr(billingAccountID),
			ProjectId:      pulumi.String(gcpProjectID),
			OrgId:          pulumi.String(googleCfg.Require("org")),
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
		}

		bucketObject, err := storage.NewBucketObject(ctx, "ingest", bucketObjectArgs)
		if err != nil {
			return err
		}

		bucketLifecycle := storage.BucketLifecycleRuleArray{
			&storage.BucketLifecycleRuleArgs{
				Action:    storage.BucketLifecycleRuleActionArgs{Type: pulumi.String("Delete")},
				Condition: storage.BucketLifecycleRuleConditionArgs{Age: pulumi.Int(cleanBucketAfterDays)},
			},
		}

		ingestBucket, err := storage.NewBucket(ctx, fmt.Sprintf("%s-ingest", gcpProjectID), &storage.BucketArgs{
			Location:                 pulumi.String("EUROPE-WEST3"),
			Project:                  pulumi.String(gcpProjectID),
			UniformBucketLevelAccess: pulumi.BoolPtr(false),
			LifecycleRules:           &bucketLifecycle,
		}, pulumi.DependsOn([]pulumi.Resource{project}))
		if err != nil {
			return err
		}

		outputBucket, err := storage.NewBucket(ctx, fmt.Sprintf("%s-output", gcpProjectID), &storage.BucketArgs{
			Location:                 pulumi.String("EUROPE-WEST3"),
			Project:                  pulumi.String(gcpProjectID),
			UniformBucketLevelAccess: pulumi.BoolPtr(false),
			LifecycleRules:           &bucketLifecycle,
		}, pulumi.DependsOn([]pulumi.Resource{project}))
		if err != nil {
			return err
		}

		functionEnv := pulumi.Map{
			"FUNCTION_KEY":  pulumi.String(functionKey),
			"INGEST_BUCKET": ingestBucket.Name,
			"RESULT_BUCKET": outputBucket.Name,
		}

		// Set arguments for creating the function resource.
		argsIngestFunc := &cloudfunctions.FunctionArgs{
			SourceArchiveBucket:  codeBucket.Name,
			Runtime:              pulumi.String("go113"),
			SourceArchiveObject:  bucketObject.Name,
			EntryPoint:           pulumi.String("Ingest"),
			TriggerHttp:          pulumi.Bool(true),
			AvailableMemoryMb:    pulumi.Int(128),
			Project:              pulumi.String(gcpProjectID),
			EnvironmentVariables: functionEnv,
		}

		// Create the function using the args.
		ingestFunc, err := cloudfunctions.NewFunction(ctx, "ingest", argsIngestFunc, pulumi.DependsOn(
			[]pulumi.Resource{
				bucketObject,
				project,
				cfAPI,
			},
		))
		if err != nil {
			return err
		}

		// Allow anyone to invoke the function
		_, err = cloudfunctions.NewFunctionIamMember(ctx, "invoker", &cloudfunctions.FunctionIamMemberArgs{
			Project:       ingestFunc.Project,
			Region:        ingestFunc.Region,
			CloudFunction: ingestFunc.Name,
			Role:          pulumi.String("roles/cloudfunctions.invoker"),
			Member:        pulumi.String("allUsers"),
		})

		if err != nil {
			return err
		}

		// Set arguments for creating the function resource.
		argsResultFunc := &cloudfunctions.FunctionArgs{
			SourceArchiveBucket:  codeBucket.Name,
			Runtime:              pulumi.String("go113"),
			SourceArchiveObject:  bucketObject.Name,
			EntryPoint:           pulumi.String("ProcessResults"),
			TriggerHttp:          pulumi.Bool(true),
			AvailableMemoryMb:    pulumi.Int(512),
			Project:              pulumi.String(gcpProjectID),
			EnvironmentVariables: functionEnv,
		}

		// Create the function using the args.
		resultFunc, err := cloudfunctions.NewFunction(ctx, "resultFunc", argsResultFunc, pulumi.DependsOn(
			[]pulumi.Resource{
				bucketObject,
				project,
				cfAPI,
			},
		))
		if err != nil {
			return err
		}

		// Allow anyone to invoke the function
		_, err = cloudfunctions.NewFunctionIamMember(ctx, "resultFuncInvoker", &cloudfunctions.FunctionIamMemberArgs{
			Project:       resultFunc.Project,
			Region:        resultFunc.Region,
			CloudFunction: resultFunc.Name,
			Role:          pulumi.String("roles/cloudfunctions.invoker"),
			Member:        pulumi.String("allUsers"),
		})

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

		if err != nil {
			return err
		}

		_, err = storage.NewBucketACL(ctx, "output_store_acl", &storage.BucketACLArgs{
			Bucket:       outputBucket.Name,
			RoleEntities: bucketPerms,
		})

		if err != nil {
			return err
		}

		codeBucketPerms := pulumi.StringArray{
			pulumi.Sprintf("OWNER:user-%s", pulumiServiceAccount),
			pulumi.Sprintf("READER:user-%s@appspot.gserviceaccount.com", project.ProjectId),
			pulumi.Sprintf("WRITER:user-%s@appspot.gserviceaccount.com", project.ProjectId),
		}

		_, err = storage.NewBucketACL(ctx, "code_store_acl", &storage.BucketACLArgs{
			Bucket:       codeBucket.Name,
			RoleEntities: codeBucketPerms,
		})

		_, err = cloudscheduler.NewJob(ctx, "resultsJob", &cloudscheduler.JobArgs{
			HttpTarget: &cloudscheduler.JobHttpTargetArgs{
				HttpMethod: pulumi.String("GET"),
				Uri:        resultFunc.HttpsTriggerUrl.ApplyString(appendFunctionKey),
			},
			AttemptDeadline: pulumi.String("320s"),
			Description:     pulumi.String("Collect transcription results"),
			RetryConfig: &cloudscheduler.JobRetryConfigArgs{
				MaxDoublings:       pulumi.Int(2),
				MaxRetryDuration:   pulumi.String("600s"),
				MinBackoffDuration: pulumi.String("60s"),
				RetryCount:         pulumi.Int(3),
			},

			Schedule: pulumi.String("*/15 * * * *"), // Every 15 minutes
			TimeZone: pulumi.String("Europe/Oslo"),
		})

		if err != nil {
			return err
		}

		// Export the DNS name of the bucket
		ctx.Export("ingestBucket", ingestBucket.Url)
		ctx.Export("outputBucket", outputBucket.Url)
		ctx.Export("ingestTrigger", ingestFunc.HttpsTriggerUrl)
		ctx.Export("resultTrigger", resultFunc.HttpsTriggerUrl)
		return nil
	})
}
