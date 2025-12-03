package journey

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	logging "github.com/konflux-ci/e2e-tests/tests/load-tests/pkg/logging"
	types "github.com/konflux-ci/e2e-tests/tests/load-tests/pkg/types"

	constants "github.com/konflux-ci/e2e-tests/pkg/constants"

	framework "github.com/konflux-ci/e2e-tests/pkg/framework"

	utils "github.com/konflux-ci/e2e-tests/pkg/utils"

	appstudioApi "github.com/konflux-ci/application-api/api/v1alpha1"

	pipeline "github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
)

// Parse PR number out of PR url
func getPRNumberFromPRUrl(prUrl string) (int, error) {
	regex := regexp.MustCompile(`/([0-9]+)/?$`)
	match := regex.FindStringSubmatch(prUrl)
	if match == nil {
		return 0, fmt.Errorf("Failed to parse PR number out of url %s", prUrl)
	}

	prNumber, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, fmt.Errorf("Failed to convert PR number %s to int: %v", match[1], err)
	}

	return prNumber, nil
}

// Get PR URL from PaC component annotation "build.appstudio.openshift.io/status"
func getPaCPull(annotations map[string]string) (string, error) {
	var buildStatusAnn string = "build.appstudio.openshift.io/status"
	var buildStatusValue string
	var buildStatusMap map[string]interface{}

	// Get annotation we are interested in
	buildStatusValue, exists := annotations[buildStatusAnn]
	if !exists {
		return "", nil
	}

	// Parse JSON
	err := json.Unmarshal([]byte(buildStatusValue), &buildStatusMap)
	if err != nil {
		return "", fmt.Errorf("Error unmarshalling JSON: %v", err)
	}

	// Access the nested value using type assertion
	if pac, ok := buildStatusMap["pac"].(map[string]interface{}); ok {
		var data string
		var ok bool

		// Example: '{"pac":{"state":"enabled","merge-url":"https://github.com/rhtap-test-local/multi-platform-test-test-rhtap-1/pull/1","configuration-time":"Thu, 23 May 2024 07:06:43 UTC"},"message":"done"}'

		// Check "state" is "enabled"
		if data, ok = pac["state"].(string); ok {
			if data != "enabled" {
				return "", fmt.Errorf("Incorrect state: %s", buildStatusValue)
			}
		} else {
			return "", fmt.Errorf("Failed parsing state: %s", buildStatusValue)
		}

		// Get "merge-url"
		if data, ok = pac["merge-url"].(string); ok {
			logging.Logger.Debug("Found PaC merge request URL: %s", data)
			return data, nil
		} else {
			return "", fmt.Errorf("Failed parsing state: %s", buildStatusValue)
		}
	} else {
		return "", fmt.Errorf("Failed parsing: %s", buildStatusValue)
	}
}

func createComponent(f *framework.Framework, namespace, repoUrl, repoRevision, containerContext, containerFile, buildPipelineName, buildPipelineSelector, appName string, componentIndex int, mintmakerDisabled bool) (string, error) {
	name := fmt.Sprintf("%s-comp-%d", appName, componentIndex)

	logging.Logger.Debug("Creating component %s in namespace %s", name, namespace)

	// Prepare annotations to add to component
	annotationsMap := constants.DefaultDockerBuildPipelineBundleAnnotation
	if buildPipelineSelector != "" {
		// Custom build pipeline selector
		annotationsMap["build.appstudio.openshift.io/pipeline"] = fmt.Sprintf(`{"name": "%s", "bundle": "%s"}`, buildPipelineName, buildPipelineSelector)
	}
	if mintmakerDisabled {
		// Stop Mintmaker creating update PRs for your component
		for key, value := range constants.ComponentMintmakerDisabledAnnotation {
			annotationsMap[key] = value
		}
	}

	// Configure image-controller to configure PaC
	for key, value := range constants.ComponentPaCRequestAnnotation {
		annotationsMap[key] = value
	}

	componentObj := appstudioApi.ComponentSpec{
		ComponentName: name,
		Source: appstudioApi.ComponentSource{
			ComponentSourceUnion: appstudioApi.ComponentSourceUnion{
				GitSource: &appstudioApi.GitSource{
					URL:           repoUrl,
					Revision:      repoRevision,
					Context:       containerContext,
					DockerfileURL: containerFile,
				},
			},
		},
	}

	_, err := f.AsKubeDeveloper.HasController.CreateComponent(componentObj, namespace, "", "", appName, false, annotationsMap)
	if err != nil {
		return "", fmt.Errorf("Unable to create the Component %s: %v", name, err)
	}
	return name, nil
}

func validateComponent(f *framework.Framework, namespace, name string) error {
	interval := time.Second * 10
	timeout := time.Minute * 30

	// TODO It would be much better to watch this resource instead querying it
	err := utils.WaitUntilWithInterval(func() (done bool, err error) {
		comp, err := f.AsKubeDeveloper.HasController.GetComponent(name, namespace)
		if err != nil {
			logging.Logger.Debug("Unable to get component %s in namespace %s for its annotations: %v", name, namespace, err)
			return false, nil
		}

		// If build.appstudio.openshift.io/request annotation is gone, component finished onboarding
		_, ok := comp.Annotations["build.appstudio.openshift.io/request"]
		if ! ok {
			logging.Logger.Debug("Finished onboarding of component %s in namespace %s", name, namespace)
			return true, nil
		}

		// If it is still there, build.appstudio.openshift.io/status will have a reason
		val, ok := comp.Annotations["build.appstudio.openshift.io/status"]
		if ok {
			logging.Logger.Debug("Onboarding of a component %s in namespace %s not finished yet: %s", name, namespace, val)
		} else {
			logging.Logger.Debug("Onboarding of a component %s in namespace %s not started yet", name, namespace)
		}

		return false, nil
	}, interval, timeout)

	return err
}

func getPaCPullNumber(f *framework.Framework, namespace, name string) (int, error) {
	interval := time.Second * 20
	timeout := time.Minute * 15
	var comp *appstudioApi.Component
	var pull string
	var pullNumber int

	// TODO It would be much better to watch this resource for a condition
	err := utils.WaitUntilWithInterval(func() (done bool, err error) {
		comp, err = f.AsKubeDeveloper.HasController.GetComponent(name, namespace)
		if err != nil {
			logging.Logger.Debug("Unable to get created Component %s in namespace %s: %v", name, namespace, err)
			return false, nil
		}

		// Check for right annotation
		pull, err = getPaCPull(comp.Annotations)
		if err != nil {
			logging.Logger.Debug("PaC component %s in namespace %s failed on PR annotation: %v", name, namespace, err)
			return false, nil
		}
		if pull == "" {
			logging.Logger.Debug("PaC component %s in namespace %s do not have PR yet", name, namespace)
			return false, nil
		}

		return true, nil
	}, interval, timeout)
	if err != nil {
		return -1, fmt.Errorf("Unable to get PaC pull number for component %s in namespace %s: %v", name, namespace, err)
	}

	// Get merge request number
	pullNumber, err = getPRNumberFromPRUrl(pull)
	if err != nil {
		return -1, fmt.Errorf("Parsing merge request number failed: %+v", err)
	}

	return pullNumber, err
}

// Link all pipeline imagePullSecrets (needed to pull images used by tasks) to build service account
func configurePipelineImagePullSecrets(f *framework.Framework, namespace, component string, secrets []string) error {
	logging.Logger.Debug("Configuring %d imagePullSecrets for component build task images for component %s", len(secrets), component)

	component_sa := "build-pipeline-" + component
	for _, secret := range secrets {
		err := f.AsKubeAdmin.CommonController.LinkSecretToServiceAccount(namespace, secret, component_sa, true)
		if err != nil {
			return fmt.Errorf("Unable to add secret %s to service account %s: %v", secret, component_sa, err)
		}
	}

	return nil
}

func listPipelineRunsWithTimeout(f *framework.Framework, namespace, appName, compName, sha string, expectedCount int) (*[]pipeline.PipelineRun, error) {
	var prs *[]pipeline.PipelineRun
	var err error

	interval := time.Second * 20
	timeout := time.Minute * 30

	err = utils.WaitUntilWithInterval(func() (done bool, err error) {
		prs, err = f.AsKubeDeveloper.HasController.GetComponentPipelineRunsWithType(compName, appName, namespace, "build", sha, "")
		if err != nil {
			logging.Logger.Debug("Waiting for PipelineRun for component %s in namespace %s", compName, namespace)
			return false, nil
		}
		if len(*prs) < expectedCount {
			logging.Logger.Debug("Not enough PipelineRuns for component %s in namespace %s: %d/%d", compName, namespace, len(*prs), expectedCount)
			return false, nil
		}
		return true, nil
	}, interval, timeout)
	if err != nil {
		return nil, fmt.Errorf("Unable to list PipelineRuns for component %s in namespace %s: %v", compName, namespace, err)
	}

	logging.Logger.Debug("Found %d/%d PipelineRuns matching %s/%s/%s/%s", len(*prs), expectedCount, namespace, appName, compName, sha)
	return prs, nil
}

func listAndDeletePipelineRunsWithTimeout(f *framework.Framework, namespace, appName, compName, sha string, expectedCount int) error {
	var prs *[]pipeline.PipelineRun
	var err error

	prs, err = listPipelineRunsWithTimeout(f, namespace, appName, compName, sha, expectedCount)
	if err != nil {
		return err
	}
	for _, pr := range *prs {
		err = f.AsKubeDeveloper.TektonController.DeletePipelineRunIgnoreFinalizers(namespace, pr.Name)
		if err != nil {
			return fmt.Errorf("Error when deleting PipelineRun %s in namespace %s: %v", pr.Name, namespace, err)
		}
		logging.Logger.Debug("Deleted PipelineRun %s/%s", namespace, pr.Name)
	}

	return nil
}

// This handles post-component creation tasks for multi-arch PaC workflow
func utilityRepoTemplatingComponentCleanup(f *framework.Framework, namespace, appName, compName, repoUrl, repoRev, sourceRepo, sourceRepoDir string, mergeReqNum int, placeholders *map[string]string) error {
	var err error

	// Delete on-pull-request default pipeline run
	err = listAndDeletePipelineRunsWithTimeout(f, namespace, appName, compName, "", 1)
	if err != nil {
		return fmt.Errorf("Error deleting on-pull-request default PipelineRun in namespace %s: %v", namespace, err)
	}
	logging.Logger.Debug("Repo-templating workflow: Cleaned up (first cleanup) for %s/%s/%s", namespace, appName, compName)

	// Merge default PaC pipelines PR
	if strings.Contains(repoUrl, "gitlab.") {
		repoId, err := getRepoIdFromRepoUrl(repoUrl)
		if err != nil {
			return fmt.Errorf("Failed parsing repo org/name: %v", err)
		}
		_, err = f.AsKubeAdmin.CommonController.Gitlab.AcceptMergeRequest(repoId, mergeReqNum)
		if err != nil {
			return fmt.Errorf("Merging %d failed: %v", mergeReqNum, err)
		}
	} else {
		repoName, err := getRepoNameFromRepoUrl(repoUrl)
		if err != nil {
			return fmt.Errorf("Failed parsing repo name: %v", err)
		}
		_, err = f.AsKubeAdmin.CommonController.Github.MergePullRequest(repoName, mergeReqNum)
		if err != nil {
			return fmt.Errorf("Merging %d failed: %v", mergeReqNum, err)
		}
	}
	logging.Logger.Debug("Repo-templating workflow: Merged PR %d in %s", mergeReqNum, repoUrl)

	// Delete all pipeline runs as we do not care about these
	err = listAndDeletePipelineRunsWithTimeout(f, namespace, appName, compName, "", 1)
	if err != nil {
		return fmt.Errorf("Error deleting on-push merged PipelineRun in namespace %s: %v", namespace, err)
	}
	logging.Logger.Debug("Repo-templating workflow: Cleaned up (second cleanup) for %s/%s/%s", namespace, appName, compName)

	// Template our multi-arch PaC files
	shaMap, err := templateFiles(f, repoUrl, repoRev, sourceRepo, sourceRepoDir, placeholders)
	if err != nil {
		return fmt.Errorf("Error templating PaC files: %v", err)
	}
	logging.Logger.Debug("Repo-templating workflow: Our PaC files templated in %s", repoUrl)

	// Delete pipeline run we do not care about
	for file, sha := range *shaMap {
		if !strings.HasSuffix(file, "-push.yaml") {
			err = listAndDeletePipelineRunsWithTimeout(f, namespace, appName, compName, sha, 1)
			if err != nil {
				return fmt.Errorf("Error deleting on-push merged PipelineRun in namespace %s: %v", namespace, err)
			}
		}
	}
	logging.Logger.Debug("Repo-templating workflow: Cleaned up (third cleanup) for %s/%s/%s", namespace, appName, compName)

	return nil
}

func HandleComponent(ctx *types.PerComponentContext) error {
	if ctx.ParentContext.ParentContext.Opts.JourneyReuseComponents && ctx.ParentContext.JourneyRepeatIndex > 0 {
		// This is a reused component. We need to get the name from the component from the first journey.
		// We must wait until the component's context from the first journey has the name.
		firstApplicationCtx := ctx.ParentContext.ParentContext.PerApplicationContexts[ctx.ParentContext.ApplicationIndex]
		firstComponentCtx := firstApplicationCtx.PerComponentContexts[ctx.ComponentIndex]

		interval := time.Second * 2
		timeout := time.Minute * 20

		err := utils.WaitUntilWithInterval(func() (done bool, err error) {
			if firstComponentCtx.ComponentName != "" {
				logging.Logger.Debug("Reused component name is now available: %s", firstComponentCtx.ComponentName)
				return true, nil
			}
			logging.Logger.Trace("Waiting for component name from first component thread")
			return false, nil
		}, interval, timeout)

		if err != nil {
			return logging.Logger.Fail(60, "timed out waiting for component name from first component thread: %v", err)
		}

		ctx.ComponentName = firstComponentCtx.ComponentName
		logging.Logger.Debug("Reusing component %s in thread %d-%d-%d", ctx.ComponentName, ctx.ParentContext.ParentContext.UserIndex, ctx.ParentContext.ApplicationIndex, ctx.ComponentIndex)
	}

	if ctx.ComponentName != "" {
		logging.Logger.Debug("Skipping setting up component because reusing component %s in namespace %s, triggering build with push to the repo", ctx.ComponentName, ctx.ParentContext.ParentContext.Namespace)
		_, err := doHarmlessCommit(ctx.Framework, ctx.ParentContext.ParentContext.ComponentRepoUrl, ctx.ParentContext.ParentContext.Opts.ComponentRepoRevision)
		if err != nil {
			return logging.Logger.Fail(60, "Commiting to repo for reused component %s in namespace %s failed: %v", ctx.ComponentName, ctx.ParentContext.ParentContext.Namespace, err)
		}
		return nil
	}

	if ctx.ParentContext.ParentContext.Opts.SerializeComponentOnboarding {
		logging.Logger.Debug("Waiting to create component in namespace %s", ctx.ParentContext.ParentContext.Namespace)
		ctx.ParentContext.ParentContext.Opts.SerializeComponentOnboardingLock.Lock()
	}

	var iface interface{}
	var ok bool
	var err error
	var mergeRequestNumber int

	// Create component
	iface, err = logging.Measure(
		ctx,
		createComponent,
		ctx.Framework,
		ctx.ParentContext.ParentContext.Namespace,
		ctx.ParentContext.ParentContext.ComponentRepoUrl,
		ctx.ParentContext.ParentContext.Opts.ComponentRepoRevision,
		ctx.ParentContext.ParentContext.Opts.ComponentContainerContext,
		ctx.ParentContext.ParentContext.Opts.ComponentContainerFile,
		ctx.ParentContext.ParentContext.Opts.BuildPipelineName,
		ctx.ParentContext.ParentContext.Opts.BuildPipelineSelectorBundle,
		ctx.ParentContext.ApplicationName,
		ctx.ComponentIndex,
		ctx.ParentContext.ParentContext.Opts.PipelineMintmakerDisabled,
	)
	if err != nil {
		return logging.Logger.Fail(61, "Component failed creation: %v", err)
	}

	ctx.ComponentName, ok = iface.(string)
	if !ok {
		return logging.Logger.Fail(62, "Type assertion failed on component name: %+v", iface)
	}

	// Validate component build service account created
	_, err = logging.Measure(
		ctx,
		validateComponent,
		ctx.Framework,
		ctx.ParentContext.ParentContext.Namespace,
		ctx.ComponentName,
	)
	if err != nil {
		return logging.Logger.Fail(63, "Component failed onboarding: %v", err)
	}

	if ctx.ParentContext.ParentContext.Opts.SerializeComponentOnboarding {
		ctx.ParentContext.ParentContext.Opts.SerializeComponentOnboardingLock.Unlock()
		logging.Logger.Debug("Freed lock to create another component after %s in namespace %s", ctx.ComponentName, ctx.ParentContext.ParentContext.Namespace)
	}

	// Configure imagePullSecrets needed for component build task images
	if len(ctx.ParentContext.ParentContext.Opts.PipelineImagePullSecrets) > 0 {
		_, err = logging.Measure(
			ctx,
			configurePipelineImagePullSecrets,
			ctx.Framework,
			ctx.ParentContext.ParentContext.Namespace,
			ctx.ComponentName,
			ctx.ParentContext.ParentContext.Opts.PipelineImagePullSecrets,
		)
		if err != nil {
			return logging.Logger.Fail(64, "Failed to configure pipeline imagePullSecrets: %v", err)
		}
	}

	iface, err = logging.Measure(
		ctx,
		getPaCPullNumber,
		ctx.Framework,
		ctx.ParentContext.ParentContext.Namespace,
		ctx.ComponentName,
	)
	if err != nil {
		return logging.Logger.Fail(65, "Component failed validation: %v", err)
	}

	// Get merge request number
	mergeRequestNumber, ok = iface.(int)
	if !ok {
		return logging.Logger.Fail(66, "Type assertion failed on pull: %+v", iface)
	}

	// When not using repo templating, always merge PR and use only on-push PipelineRun
	if !ctx.ParentContext.ParentContext.Opts.PipelineRepoTemplating {
		// Step 1: Optionally inject build-platforms if provided
		if ctx.ParentContext.ParentContext.Opts.BuildPlatforms != "" {
			logging.Logger.Debug("Injecting build-platforms parameter into PipelineRun YAML files for PR #%d", mergeRequestNumber)
			err = InjectBuildPlatformsToYaml(
				ctx.Framework,
				ctx.ParentContext.ParentContext.ComponentRepoUrl,
				ctx.ParentContext.ParentContext.Opts.ComponentRepoRevision,
				ctx.ComponentName,
				ctx.ParentContext.ParentContext.Opts.BuildPlatforms,
				mergeRequestNumber,
			)
			if err != nil {
				return logging.Logger.Fail(68, "Failed to inject build-platforms into PipelineRun: %v", err)
			}
		} else {
			logging.Logger.Debug("No build-platforms specified, will merge PR #%d with default pipeline configuration", mergeRequestNumber)
		}

		// Step 2: First cleanup - delete initial on-pull-request PipelineRuns
		logging.Logger.Debug("Deleting initial on-pull-request PipelineRuns before merging PR")
		err = listAndDeletePipelineRunsWithTimeout(
			ctx.Framework,
			ctx.ParentContext.ParentContext.Namespace,
			ctx.ParentContext.ApplicationName,
			ctx.ComponentName,
			"", // empty sha means get all PipelineRuns for this component
			1,  // expect at least 1 PipelineRun
		)
		if err != nil {
			return logging.Logger.Fail(69, "Failed to delete initial PipelineRuns: %v", err)
		}

		// Step 3: Merge PR to trigger on-push PipelineRun
		// Add initial wait to give GitHub/GitLab time to process injection commits
		initialWait := time.Second * 15
		logging.Logger.Debug("Waiting %v for GitHub/GitLab to process injection commits before attempting merge", initialWait)
		time.Sleep(initialWait)

		// Retry merge with backoff to handle GitHub/GitLab processing time after injection commits
		logging.Logger.Debug("Merging PR %d to trigger on-push PipelineRun (with retry for processing time)", mergeRequestNumber)

		maxRetries := 5
		retryDelay := time.Second * 10
		var mergeErr error

		for attempt := 1; attempt <= maxRetries; attempt++ {
			if attempt > 1 {
				logging.Logger.Debug("Merge attempt %d/%d (waiting %v for PR to become mergeable)", attempt, maxRetries, retryDelay)
				time.Sleep(retryDelay)
			}

			if strings.Contains(ctx.ParentContext.ParentContext.ComponentRepoUrl, "gitlab.") {
				repoId, err := getRepoIdFromRepoUrl(ctx.ParentContext.ParentContext.ComponentRepoUrl)
				if err != nil {
					return logging.Logger.Fail(70, "Failed parsing repo org/name: %v", err)
				}
				_, mergeErr = ctx.Framework.AsKubeAdmin.CommonController.Gitlab.AcceptMergeRequest(repoId, mergeRequestNumber)
			} else {
				repoName, err := getRepoNameFromRepoUrl(ctx.ParentContext.ParentContext.ComponentRepoUrl)
				if err != nil {
					return logging.Logger.Fail(72, "Failed parsing repo name: %v", err)
				}
				_, mergeErr = ctx.Framework.AsKubeAdmin.CommonController.Github.MergePullRequest(repoName, mergeRequestNumber)
			}

			if mergeErr == nil {
				logging.Logger.Debug("PR %d merged successfully on attempt %d", mergeRequestNumber, attempt)
				break
			}

			// Check if error is retryable (405 "not mergeable", 409 "out of date", or similar)
			if strings.Contains(mergeErr.Error(), "405") || strings.Contains(mergeErr.Error(), "not mergeable") ||
				strings.Contains(mergeErr.Error(), "409") || strings.Contains(mergeErr.Error(), "out of date") {
				logging.Logger.Debug("PR %d not ready to merge yet (attempt %d/%d): %v", mergeRequestNumber, attempt, maxRetries, mergeErr)
				if attempt == maxRetries {
					return logging.Logger.Fail(73, "Failed to merge PR %d after %d attempts: %v", mergeRequestNumber, maxRetries, mergeErr)
				}
				// Continue to retry
			} else {
				// Non-retryable error, fail immediately
				return logging.Logger.Fail(73, "Failed to merge PR %d with non-retryable error: %v", mergeRequestNumber, mergeErr)
			}
		}

		logging.Logger.Debug("PR merged successfully, on-push PipelineRun triggered. Now cleaning up cancelled on-pull-request PipelineRuns")

		// Step 4: Second cleanup - delete all cancelled on-pull-request PipelineRuns after merge
		// This removes any on-pull-request PipelineRuns (from injection commits or still running)
		// keeping only the new on-push PipelineRun
		err = listAndDeletePipelineRunsWithTimeout(
			ctx.Framework,
			ctx.ParentContext.ParentContext.Namespace,
			ctx.ParentContext.ApplicationName,
			ctx.ComponentName,
			"", // empty sha means get all PipelineRuns for this component
			1,  // expect at least 1 cancelled on-pull-request PipelineRun
		)
		if err != nil {
			logging.Logger.Debug("Second cleanup after merge had no PipelineRuns to delete (this is OK): %v", err)
		} else {
			logging.Logger.Debug("Cleaned up cancelled on-pull-request PipelineRuns after merge for %s/%s/%s", ctx.ParentContext.ParentContext.Namespace, ctx.ParentContext.ApplicationName, ctx.ComponentName)
		}

		logging.Logger.Debug("Ready to proceed - only on-push PipelineRun should remain")
	}

	// If this is supposed to be a multi-arch build, we do not care about
	// current build, we just merge the PR, update pipelines and trigger
	// actual multi-arch build
	if ctx.ParentContext.ParentContext.Opts.PipelineRepoTemplating {
		// Placeholders for template multi-arch PaC pipeline files
		placeholders := &map[string]string{
			"NAMESPACE":       ctx.ParentContext.ParentContext.Namespace,
			"QUAY_REPO":       ctx.ParentContext.ParentContext.Opts.QuayRepo,
			"APPLICATION":     ctx.ParentContext.ApplicationName,
			"COMPONENT":       ctx.ComponentName,
			"BRANCH":          ctx.ParentContext.ParentContext.Opts.ComponentRepoRevision,
			"REPOURL":         ctx.ParentContext.ParentContext.ComponentRepoUrl,
			"BUILD_PLATFORMS": ctx.ParentContext.ParentContext.Opts.BuildPlatforms,
		}

		// Skip what we do not care about, merge PR, graft pipeline yamls
		_, err = logging.Measure(
			ctx,
			utilityRepoTemplatingComponentCleanup,
			ctx.Framework,
			ctx.ParentContext.ParentContext.Namespace,
			ctx.ParentContext.ApplicationName,
			ctx.ComponentName,
			ctx.ParentContext.ParentContext.ComponentRepoUrl,
			ctx.ParentContext.ParentContext.Opts.ComponentRepoRevision,
			ctx.ParentContext.ParentContext.Opts.PipelineRepoTemplatingSource,
			ctx.ParentContext.ParentContext.Opts.PipelineRepoTemplatingSourceDir,
			mergeRequestNumber,
			placeholders,
		)
		if err != nil {
			return logging.Logger.Fail(67, "Repo-templating workflow component cleanup failed: %v", err)
		}

	}

	return nil
}
