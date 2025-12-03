package journey

import "fmt"
import "strings"
import "regexp"
import "time"

import logging "github.com/konflux-ci/e2e-tests/tests/load-tests/pkg/logging"
import types "github.com/konflux-ci/e2e-tests/tests/load-tests/pkg/types"

import framework "github.com/konflux-ci/e2e-tests/pkg/framework"
import github "github.com/google/go-github/v44/github"
import utils "github.com/konflux-ci/e2e-tests/pkg/utils"
import "sigs.k8s.io/yaml"

var fileList = []string{"COMPONENT-pull-request.yaml", "COMPONENT-push.yaml"}

// Parse repo name out of repo url
func getRepoNameFromRepoUrl(repoUrl string) (string, error) {
	// Answer taken from https://stackoverflow.com/questions/7124778/how-can-i-match-anything-up-until-this-sequence-of-characters-in-a-regular-exp
	// Tested with these input data:
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample.git/, match[1]: nodejs-devfile-sample
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample.git, match[1]: nodejs-devfile-sample
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample/, match[1]: nodejs-devfile-sample
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample, match[1]: nodejs-devfile-sample
	//   repoUrl: https://gitlab.example.com/abc/nodejs-devfile-sample, match[1]: nodejs-devfile-sample
	//   repoUrl: https://gitlab.example.com/abc/def/nodejs-devfile-sample, match[1]: nodejs-devfile-sample
	regex := regexp.MustCompile(`/([^/]+?)(.git)?/?$`)
	match := regex.FindStringSubmatch(repoUrl)
	if match != nil {
		return match[1], nil
	} else {
		return "", fmt.Errorf("Failed to parse repo name out of url %s", repoUrl)
	}
}

// Parse repo organization out of repo url
func getRepoOrgFromRepoUrl(repoUrl string) (string, error) {
	// Answer taken from https://stackoverflow.com/questions/7124778/how-can-i-match-anything-up-until-this-sequence-of-characters-in-a-regular-exp
	// Tested with these input data:
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample.git/, match[1]: abc
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample.git, match[1]: abc
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample/, match[1]: abc
	//   repoUrl: https://github.com/abc/nodejs-devfile-sample, match[1]: abc
	//   repoUrl: https://gitlab.example.com/abc/nodejs-devfile-sample, match[1]: abc
	//   repoUrl: https://gitlab.example.com/abc/def/nodejs-devfile-sample, match[1]: abc/def
	regex := regexp.MustCompile(`^[^/]+://[^/]+/(.*)/.+(.git)?/?$`)
	match := regex.FindStringSubmatch(repoUrl)
	if match != nil {
		return match[1], nil
	} else {
		return "", fmt.Errorf("Failed to parse repo org out of url %s", repoUrl)
	}
}

// Parse repo ID (<organization>/<name>) out of repo url
func getRepoIdFromRepoUrl(repoUrl string) (string, error) {
	repoOrgName, err := getRepoOrgFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}
	repoName, err := getRepoNameFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}
	return repoOrgName + "/" + repoName, nil
}

// Get file content from repository, no matter if on GitLab or GitHub
func getRepoFileContent(f *framework.Framework, repoUrl, repoRevision, fileName string) (string, error) {
	var fileContent string

	repoName, err := getRepoNameFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}
	repoOrgName, err := getRepoOrgFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}

	if strings.Contains(repoUrl, "gitlab.") {
		fileContent, err = f.AsKubeAdmin.CommonController.Gitlab.GetFile(repoOrgName + "/" + repoName, fileName, repoRevision)
		if err != nil {
			return "", fmt.Errorf("Failed to get file %s from repo %s revision %s: %v", fileName, repoOrgName + "/" + repoName, repoRevision, err)
		}
	} else {
		fileResponse, err := f.AsKubeAdmin.CommonController.Github.GetFileWithOrg(repoOrgName, repoName, fileName, repoRevision)
		if err != nil {
			return "", fmt.Errorf("Failed to get file %s from repo %s revision %s: %v", fileName, repoName, repoRevision, err)
		}

		fileContent, err = fileResponse.GetContent()
		if err != nil {
			return "", err
		}
	}

	return fileContent, nil
}

// Update file content in repository, no matter if on GitLab or GitHub
func updateRepoFileContent(f *framework.Framework, repoUrl, repoRevision, fileName, fileContent string) (string, error) {
	var commitSha string

	repoName, err := getRepoNameFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}
	repoOrgName, err := getRepoOrgFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}

	if strings.Contains(repoUrl, "gitlab.") {
		commitSha, err = f.AsKubeAdmin.CommonController.Gitlab.UpdateFile(repoOrgName + "/" + repoName, fileName, fileContent, repoRevision)
		if err != nil {
			return "", fmt.Errorf("Failed to update file %s in repo %s revision %s: %v", fileName, repoOrgName + "/" + repoName, repoRevision, err)
		}
	} else {
		fileResponse, err := f.AsKubeAdmin.CommonController.Github.GetFile(repoName, fileName, repoRevision)
		if err != nil {
			return "", fmt.Errorf("Failed to get file %s from repo %s revision %s: %v", fileName, repoName, repoRevision, err)
		}

		repoContentResponse, err := f.AsKubeAdmin.CommonController.Github.UpdateFile(repoName, fileName, fileContent, repoRevision, *fileResponse.SHA)
		if err != nil {
			return "", fmt.Errorf("Failed to update file %s in repo %s revision %s: %v", fileName, repoName, repoRevision, err)
		}

		commitSha = *repoContentResponse.Commit.SHA
	}

	return commitSha, nil
}

// Template file from source repo and dir to '.tekton/...' in component repo, expanding placeholders (even in file name), no matter if on GitLab or GitHub
// Returns SHA of the commit
func templateRepoFile(f *framework.Framework, repoUrl, repoRevision, sourceRepo, sourceRepoDir, fileName string, placeholders *map[string]string) (string, error) {
	fileContent, err := getRepoFileContent(f, sourceRepo, "main", sourceRepoDir + fileName)
	if err != nil {
		return "", err
	}

	for key, value := range *placeholders {
		fileContent = strings.ReplaceAll(fileContent, key, value)
		fileName = strings.ReplaceAll(fileName, key, value)
	}

	commitSha, err := updateRepoFileContent(f, repoUrl, repoRevision, ".tekton/" + fileName, fileContent)
	if err != nil {
		return "", err
	}

	return commitSha, nil
}

// Fork repository and return forked repo URL
func ForkRepo(f *framework.Framework, repoUrl, repoRevision, suffix, targetOrgName string) (string, error) {
	// For PaC testing, let's template repo and return forked repo name
	var forkRepo *github.Repository
	var sourceName string
	var sourceOrgName string
	var targetName string
	var err error

	// Parse just repo name and org out of input repo url and construct target repo name
	sourceName, err = getRepoNameFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}
	sourceOrgName, err = getRepoOrgFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}

	targetName = fmt.Sprintf("%s-%s", sourceName, suffix)

	if strings.Contains(repoUrl, "gitlab.") {
		// Cleanup if it already exists
		err = f.AsKubeAdmin.CommonController.Gitlab.DeleteRepositoryIfExists(targetOrgName + "/" + targetName)
		if err != nil {
			return "", err
		}

		// Create fork and make sure it appears
		forkedRepoURL, err := f.AsKubeAdmin.CommonController.Gitlab.ForkRepository(sourceOrgName, sourceName, targetOrgName, targetName)
		if err != nil {
			return "", err
		}

		return forkedRepoURL.WebURL, nil
	} else {
		// Cleanup if it already exists
		err = f.AsKubeAdmin.CommonController.Github.DeleteRepositoryIfExists(targetName)
		if err != nil {
			return "", err
		}

		// Create fork and make sure it appears
		err = utils.WaitUntilWithInterval(func() (done bool, err error) {
			forkRepo, err = f.AsKubeAdmin.CommonController.Github.ForkRepositoryWithOrgs(sourceOrgName, sourceName, targetOrgName, targetName)
			if err != nil {
				logging.Logger.Debug("Repo forking failed, trying again: %v", err)
				return false, nil
			}
			return true, nil
		}, time.Second * 20, time.Minute * 10)
		if err != nil {
			return "", err
		}

		return forkRepo.GetHTMLURL(), nil
	}
}

// Template PaC files
func templateFiles(f *framework.Framework, repoUrl, repoRevision, sourceRepo, sourceRepoDir string, placeholders *map[string]string) (*map[string]string, error) {
	// Template files we care about
	shaMap := &map[string]string{}
	for _, file := range fileList {
		sha, err := templateRepoFile(f, repoUrl, repoRevision, sourceRepo, sourceRepoDir, file, placeholders)
		if err != nil {
			return nil, err
		}
		logging.Logger.Debug("Templated file %s with commit %s", file, sha)
		(*shaMap)[file] = sha
	}

	return shaMap, nil
}

// doHarmlessCommit creates or updates file "just-trigger-build" with current timestamp and commits it
func doHarmlessCommit(f *framework.Framework, repoUrl, repoRevision string) (string, error) {
	fileName := "just-trigger-build"
	var fileContent string
	var sha *string
	var commitSha string

	repoName, err := getRepoNameFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}
	repoOrgName, err := getRepoOrgFromRepoUrl(repoUrl)
	if err != nil {
		return "", err
	}

	if strings.Contains(repoUrl, "gitlab.") {
		// For gitlab, we can get file content. If it fails, we assume it doesn't exist.
		// The UpdateFile API for gitlab creates the file if it doesn't exist.
		existingContent, err := f.AsKubeAdmin.CommonController.Gitlab.GetFile(repoOrgName+"/"+repoName, fileName, repoRevision)
		if err != nil {
			logging.Logger.Debug("Failed to get file %s from repo %s, assuming it does not exist: %v", fileName, repoUrl, err)
			fileContent = ""
		} else {
			fileContent = existingContent
		}
		fileContent += fmt.Sprintf("\n# %s", time.Now().String())

		commitSha, err = f.AsKubeAdmin.CommonController.Gitlab.UpdateFile(repoOrgName+"/"+repoName, fileName, fileContent, repoRevision)
		if err != nil {
			return "", fmt.Errorf("Failed to update file %s in repo %s revision %s: %v", fileName, repoOrgName+"/"+repoName, repoRevision, err)
		}
	} else {
		// For github, we need to get SHA if file exists.
		fileResponse, err := f.AsKubeAdmin.CommonController.Github.GetFile(repoName, fileName, repoRevision)
		if err != nil {
			// Assuming error means not found.
			logging.Logger.Debug("File %s not found in repo %s, will create it.", fileName, repoUrl)
			fileContent = ""
			sha = nil
		} else {
			existingContent, err := fileResponse.GetContent()
			if err != nil {
				return "", err
			}
			fileContent = existingContent
			sha = fileResponse.SHA
		}

		fileContent += fmt.Sprintf("\n# %s", time.Now().String())

		if sha == nil {
			// We have to assume a CreateFile function exists in the framework's github controller
			repoContentResponse, err := f.AsKubeAdmin.CommonController.Github.CreateFile(repoName, fileName, fileContent, repoRevision)
			if err != nil {
				return "", fmt.Errorf("Failed to create file %s in repo %s: %v", fileName, repoUrl, err)
			}
			commitSha = *repoContentResponse.Commit.SHA
		} else {
			repoContentResponse, err := f.AsKubeAdmin.CommonController.Github.UpdateFile(repoName, fileName, fileContent, repoRevision, *sha)
			if err != nil {
				return "", fmt.Errorf("Failed to update file %s in repo %s: %v", fileName, repoUrl, err)
			}
			commitSha = *repoContentResponse.Commit.SHA
		}
	}
	return commitSha, nil
}

// Add build-platforms parameter to PipelineRun YAML file
func addBuildPlatformsToPipelineRun(fileContent, buildPlatforms string) (string, error) {
	if buildPlatforms == "" {
		return fileContent, nil
	}

	// Parse YAML into a map
	var pipelineRun map[string]interface{}
	if err := yaml.Unmarshal([]byte(fileContent), &pipelineRun); err != nil {
		return "", fmt.Errorf("failed to parse PipelineRun YAML: %v", err)
	}

	// Navigate to spec.params
	spec, ok := pipelineRun["spec"].(map[string]interface{})
	if !ok {
		logging.Logger.Debug("No 'spec' section found in PipelineRun YAML")
		return fileContent, nil
	}

	params, ok := spec["params"].([]interface{})
	if !ok {
		logging.Logger.Debug("No 'params' section found in spec")
		return fileContent, nil
	}

	// Split platforms by comma and trim whitespace
	platformList := strings.Split(buildPlatforms, ",")
	platforms := make([]interface{}, len(platformList))
	for i := range platformList {
		platforms[i] = strings.TrimSpace(platformList[i])
	}

	// Check if build-platforms already exists and update it, or add new parameter
	found := false
	for _, param := range params {
		if paramMap, ok := param.(map[string]interface{}); ok {
			if name, ok := paramMap["name"].(string); ok && name == "build-platforms" {
				// Update existing build-platforms parameter
				paramMap["value"] = platforms
				found = true
				logging.Logger.Debug("Updated existing build-platforms parameter in PipelineRun YAML")
				break
			}
		}
	}

	// If not found, add to the beginning of params array
	if !found {
		buildPlatformsParam := map[string]interface{}{
			"name":  "build-platforms",
			"value": platforms,
		}
		spec["params"] = append([]interface{}{buildPlatformsParam}, params...)
		logging.Logger.Debug("Added new build-platforms parameter to PipelineRun YAML")
	}

	// Marshal back to YAML
	modifiedYAML, err := yaml.Marshal(pipelineRun)
	if err != nil {
		return "", fmt.Errorf("failed to marshal modified PipelineRun YAML: %v", err)
	}

	return string(modifiedYAML), nil
}

// Inject build-platforms parameter into PR's PipelineRun files
func InjectBuildPlatformsToYaml(f *framework.Framework, repoUrl, repoRevision, componentName, buildPlatforms string, prNumber int) error {
	if buildPlatforms == "" {
		return nil
	}

	// Get PR details to find the source branch where PaC files actually exist
	repoName, err := getRepoNameFromRepoUrl(repoUrl)
	if err != nil {
		return fmt.Errorf("Failed to parse repo name: %v", err)
	}
	repoOrgName, err := getRepoOrgFromRepoUrl(repoUrl)
	if err != nil {
		return fmt.Errorf("Failed to parse repo org: %v", err)
	}

	var prBranch string
	if strings.Contains(repoUrl, "gitlab.") {
		// For GitLab, get merge request details using client directly
		mr, _, err := f.AsKubeAdmin.CommonController.Gitlab.GetClient().MergeRequests.GetMergeRequest(repoOrgName+"/"+repoName, prNumber, nil)
		if err != nil {
			return fmt.Errorf("Failed to get GitLab MR %d: %v", prNumber, err)
		}
		prBranch = mr.SourceBranch
	} else {
		// For GitHub, get PR details to find the branch with PaC files
		pr, err := f.AsKubeAdmin.CommonController.Github.GetPullRequest(repoName, prNumber)
		if err != nil {
			return fmt.Errorf("Failed to get GitHub PR %d from repo %s: %v", prNumber, repoName, err)
		}
		prBranch = pr.Head.GetRef()
	}

	logging.Logger.Debug("Found PR #%d source branch: %s (PaC files exist here, not in %s)", prNumber, prBranch, repoRevision)

	// Files to modify
	pullRequestFile := ".tekton/" + componentName + "-pull-request.yaml"
	pushFile := ".tekton/" + componentName + "-push.yaml"

	files := []string{pullRequestFile, pushFile}

	for _, fileName := range files {
		// Get current file content from PR branch (where files actually exist)
		fileContent, err := getRepoFileContent(f, repoUrl, prBranch, fileName)
		if err != nil {
			logging.Logger.Debug("Could not get file %s from branch %s, skipping: %v", fileName, prBranch, err)
			continue
		}

		// Add build-platforms parameter
		modifiedContent, err := addBuildPlatformsToPipelineRun(fileContent, buildPlatforms)
		if err != nil {
			return fmt.Errorf("Failed to add build-platforms to %s: %v", fileName, err)
		}

		// Update file in PR branch
		_, err = updateRepoFileContent(f, repoUrl, prBranch, fileName, modifiedContent)
		if err != nil {
			return fmt.Errorf("Failed to update file %s in branch %s: %v", fileName, prBranch, err)
		}

		logging.Logger.Debug("Successfully injected build-platforms into %s on branch %s", fileName, prBranch)
	}

	return nil
}

func HandleRepoForking(ctx *types.PerUserContext) error {
	var suffix string
	if ctx.Opts.Stage {
		suffix = ctx.Opts.RunPrefix + "-" + ctx.Namespace
	} else {
		suffix = ctx.Namespace
	}
	logging.Logger.Debug("Forking repository %s with suffix %s to %s", ctx.Opts.ComponentRepoUrl, suffix, ctx.Opts.ForkTarget)

	forkUrl, err := ForkRepo(
		ctx.Framework,
		ctx.Opts.ComponentRepoUrl,
		ctx.Opts.ComponentRepoRevision,
		suffix,
		ctx.Opts.ForkTarget,
	)
	if err != nil {
		return logging.Logger.Fail(80, "Repo forking failed: %v", err)
	}

	logging.Logger.Info("Forked %s to %s", ctx.Opts.ComponentRepoUrl, forkUrl)

	ctx.ComponentRepoUrl = forkUrl

	return nil
}
