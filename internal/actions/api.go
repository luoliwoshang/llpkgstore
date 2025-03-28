// Package actions contains GitHub Actions helper functions for version management and repository operations.
package actions

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-github/v69/github"
	"github.com/luoliwoshang/llpkgstore/config"
	"github.com/luoliwoshang/llpkgstore/internal/actions/file"
	"github.com/luoliwoshang/llpkgstore/internal/actions/pc"
	"github.com/luoliwoshang/llpkgstore/internal/actions/versions"
)

const (
	LabelPrefix         = "branch:"
	BranchPrefix        = "release-branch."
	MappedVersionPrefix = "Release-as: "

	defaultReleaseBranch = "main"
	regexString          = `Release-as:\s%s/v(?P<major>0|[1-9]\d*)\.(?P<minor>0|[1-9]\d*)\.(?P<patch>0|[1-9]\d*)(?:-(?P<prerelease>(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9]\d*|\d*[a-zA-Z-][0-9a-zA-Z-]*))*))?(?:\+(?P<buildmetadata>[0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?`
)

// regex compiles a regular expression pattern to detect "Release-as" directives in commit messages
// Parameters:
//
//	packageName: Name of the package to format into the regex pattern
//
// Returns:
//
//	*regexp.Regexp: Compiled regular expression for version parsing
func regex(packageName string) *regexp.Regexp {
	// format: Release-as: clib/semver(with v prefix)
	// Must have one space in the end of Release-as:
	return regexp.MustCompile(fmt.Sprintf(regexString, packageName))
}

func binaryZip(packageName string) string {
	return fmt.Sprintf("%s_%s.zip", packageName, currentSuffix)
}

// DefaultClient provides GitHub API client capabilities with authentication for Actions workflows
type DefaultClient struct {
	// repo: Target repository name
	// owner: Repository owner organization/user
	// client: Authenticated GitHub API client instance
	repo   string
	owner  string
	client *github.Client
}

// NewDefaultClient initializes a new GitHub API client with authentication and repository configuration
// Uses:
//   - GitHub token from environment
//   - Repository info from GITHUB_REPOSITORY context
//
// Returns:
//
//	*DefaultClient: Configured client instance
func NewDefaultClient() *DefaultClient {
	dc := &DefaultClient{
		client: github.NewClient(nil).WithAuthToken(Token()),
	}
	dc.owner, dc.repo = Repository()
	return dc
}

// hasBranch checks existence of a specific branch in the repository
// Parameters:
//
//	branchName: Name of the branch to check
//
// Returns:
//
//	bool: True if branch exists
func (d *DefaultClient) hasBranch(branchName string) bool {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	branch, resp, err := d.client.Repositories.GetBranch(
		ctx, d.owner, d.repo, branchName, 0,
	)

	return err == nil && branch != nil &&
		resp.StatusCode == http.StatusOK
}

// associatedWithPullRequest finds all pull requests containing the specified commit
// Parameters:
//
//	sha: Commit hash to search for
//
// Returns:
//
//	[]*github.PullRequest: List of associated pull requests
func (d *DefaultClient) associatedWithPullRequest(sha string) []*github.PullRequest {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	pulls, _, err := d.client.PullRequests.ListPullRequestsWithCommit(
		ctx, d.owner, d.repo, sha, &github.ListOptions{},
	)
	must(err)
	return pulls
}

// isAssociatedWithPullRequest checks if commit belongs to a closed pull request
// Parameters:
//
//	sha: Commit hash to check
//
// Returns:
//
//	bool: True if part of closed PR
func (d *DefaultClient) isAssociatedWithPullRequest(sha string) bool {
	pulls := d.associatedWithPullRequest(sha)
	// don't use GetMerge, because GetMerge may be a mistake.
	// sometime, when a pull request is merged, GetMerge still returns false.
	// so checking pull request state is more accurate.
	return len(pulls) > 0 &&
		pulls[0].GetState() == "closed"
}

// isLegacyVersion determines if PR targets a legacy branch
// Returns:
//
//	branchName: Base branch name
//	legacy: True if branch starts with "release-branch."
func (d *DefaultClient) isLegacyVersion() (branchName string, legacy bool) {
	pullRequest, ok := GitHubEvent()["pull_request"].(map[string]any)
	var refName string
	if !ok {
		// if this actions is not triggered by pull request, fallback to call API.
		pulls := d.associatedWithPullRequest(LatestCommitSHA())
		if len(pulls) == 0 {
			panic("this commit is not associated with a pull request, this should not happen")
		}
		refName = pulls[0].GetBase().GetRef()
	} else {
		// unnecessary to check type, because currentPRCommit has been checked.
		base := pullRequest["base"].(map[string]any)
		refName = base["ref"].(string)
	}

	legacy = strings.HasPrefix(refName, BranchPrefix)
	branchName = refName
	return
}

// currentPRCommit retrieves all commits in the current pull request
// Returns:
//
//	[]*github.RepositoryCommit: List of PR commits
func (d *DefaultClient) currentPRCommit() []*github.RepositoryCommit {
	pullRequest := PullRequestEvent()
	prNumber := int(pullRequest["number"].(float64))

	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()
	// use authorized API to avoid Github RateLimit
	commits, _, err := d.client.PullRequests.ListCommits(
		ctx, d.owner, d.repo, prNumber,
		&github.ListOptions{},
	)
	must(err)
	return commits
}

// allCommits retrieves all repository commits
// Returns:
//
//	[]*github.RepositoryCommit: List of all commits
func (d *DefaultClient) allCommits() []*github.RepositoryCommit {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()
	// use authorized API to avoid Github RateLimit
	commits, _, err := d.client.Repositories.ListCommits(
		ctx, d.owner, d.repo,
		&github.CommitsListOptions{},
	)
	must(err)
	return commits
}

// removeLabel deletes a label from the repository
// Parameters:
//
//	labelName: Name of the label to remove
func (d *DefaultClient) removeLabel(labelName string) {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()
	// use authorized API to avoid Github RateLimit
	_, err := d.client.Issues.DeleteLabel(
		ctx, d.owner, d.repo, labelName,
	)
	must(err)
}

// checkMappedVersion validates PR contains valid "Release-as" version declaration
// Parameters:
//
//	packageName: Target package name for version mapping
//
// Returns:
//
//	string: Validated mapped version string
//
// Panics:
//
//	If no valid version found in PR commits
func (d *DefaultClient) checkMappedVersion(packageName string) (mappedVersion string) {
	matchMappedVersion := regex(packageName)

	for _, commit := range d.currentPRCommit() {
		message := commit.GetCommit().GetMessage()
		if mappedVersion = matchMappedVersion.FindString(message); mappedVersion != "" {
			// remove space, of course
			mappedVersion = strings.TrimSpace(mappedVersion)
			break
		}
	}

	if mappedVersion == "" {
		panic("no MappedVersion found in the PR")
	}
	return
}

// commitMessage retrieves commit details by SHA
// Parameters:
//
//	sha: Commit hash to retrieve
//
// Returns:
//
//	*github.RepositoryCommit: Commit details object
func (d *DefaultClient) commitMessage(sha string) *github.RepositoryCommit {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	commit, _, err := d.client.Repositories.GetCommit(ctx, d.owner, d.repo, sha, &github.ListOptions{})
	must(err)
	return commit
}

// mappedVersion parses the latest commit's mapped version from "Release-as" directive
// Returns:
//
//	string: Parsed version string or empty if not found
//
// Panics:
//
//	If version format is invalid
func (d *DefaultClient) mappedVersion() string {
	// get message
	message := d.commitMessage(LatestCommitSHA()).GetCommit().GetMessage()

	// parse the mapped version
	mappedVersion := regex(".*").FindString(message)
	// mapped version not found, a normal commit?
	if mappedVersion == "" {
		return ""
	}
	version := strings.TrimPrefix(mappedVersion, MappedVersionPrefix)
	if version == mappedVersion {
		panic("invalid format")
	}
	return strings.TrimSpace(version)
}

// createTag creates a new Git tag pointing to specific commit
// Parameters:
//
//	tag: Tag name (e.g. "v1.2.3")
//	sha: Target commit hash
//
// Returns:
//
//	error: Error during tag creation
func (d *DefaultClient) createTag(tag, sha string) error {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	// tag the commit
	tagRefName := tagRef(tag)
	_, _, err := d.client.Git.CreateRef(ctx, d.owner, d.repo, &github.Reference{
		Ref: &tagRefName,
		Object: &github.GitObject{
			SHA: &sha,
		},
	})

	return err
}

// createBranch creates a new branch pointing to specific commit
// Parameters:
//
//	branchName: New branch name
//	sha: Target commit hash
//
// Returns:
//
//	error: Error during branch creation
func (d *DefaultClient) createBranch(branchName, sha string) error {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	branchRefName := branchRef(branchName)
	_, _, err := d.client.Git.CreateRef(ctx, d.owner, d.repo, &github.Reference{
		Ref: &branchRefName,
		Object: &github.GitObject{
			SHA: &sha,
		},
	})

	return err
}

func (d *DefaultClient) createReleaseByTag(tag string) *github.RepositoryRelease {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	branch := defaultReleaseBranch

	makeLatest := "true"
	if _, isLegacy := d.isLegacyVersion(); isLegacy {
		makeLatest = "legacy"
	}
	generateRelease := true

	release, _, err := d.client.Repositories.CreateRelease(ctx, d.owner, d.repo, &github.RepositoryRelease{
		TagName:              &tag,
		TargetCommitish:      &branch,
		Name:                 &tag,
		MakeLatest:           &makeLatest,
		GenerateReleaseNotes: &generateRelease,
	})
	must(err)

	return release
}

func (d *DefaultClient) getReleaseByTag(tag string) *github.RepositoryRelease {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	release, _, err := d.client.Repositories.GetReleaseByTag(ctx, d.owner, d.repo, tag)
	must(err)
	// ok we get the relase entry
	return release
}

func (d *DefaultClient) uploadFileToRelease(fileName string, release *github.RepositoryRelease) error {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	fs, err := os.Open(fileName)
	must(err)
	defer fs.Close()

	_, _, err = d.client.Repositories.UploadReleaseAsset(
		ctx, d.owner, d.repo, release.GetID(),
		&github.UploadOptions{
			Name: filepath.Base(fs.Name()),
		}, fs)

	return err
}

// removeBranch deletes a branch from the repository
// Parameters:
//
//	branchName: Name of the branch to delete
//
// Returns:
//
//	error: Error during branch deletion
func (d *DefaultClient) removeBranch(branchName string) error {
	ctx, cancel := context.WithTimeout(context.TODO(), 30*time.Second)
	defer cancel()

	_, err := d.client.Git.DeleteRef(ctx, d.owner, d.repo, branchRef(branchName))

	return err
}

// checkVersion performs version validation and configuration checks
// Parameters:
//
//	ver: Version store object
//	cfg: Package configuration
func (d *DefaultClient) checkVersion(ver *versions.Versions, cfg config.LLPkgConfig) {
	// 4. Check MappedVersion
	version := d.checkMappedVersion(cfg.Upstream.Package.Name)
	_, mappedVersion := parseMappedVersion(version)

	// 5. Check version is valid
	_, isLegacy := d.isLegacyVersion()
	checkLegacyVersion(ver, cfg, mappedVersion, isLegacy)
}

// CheckPR validates PR changes and returns affected packages
// Returns:
//
//	[]string: List of affected package paths
func (d *DefaultClient) CheckPR() []string {
	// build a file path map
	pathMap := map[string][]string{}
	for _, path := range Changes() {
		dir := filepath.Dir(path)
		// initialize the dir
		pathMap[dir] = nil
	}

	var allPaths []string

	ver := versions.Read("llpkgstore.json")

	for path := range pathMap {
		// don't retrieve files from pr changes, consider about maintenance case
		files, _ := os.ReadDir(path)

		if !isValidLLPkg(files) {
			delete(pathMap, path)
			continue
		}
		// 3. Check directory name
		llpkgFile := filepath.Join(path, "llpkg.cfg")
		cfg, err := config.ParseLLPkgConfig(llpkgFile)
		if err != nil {
			panic(err)
		}
		// in our design, directory name should equal to the package name,
		// which means it's not required to be equal.
		//
		// However, at the current stage, if this is not equal, conan may panic,
		// to aovid unexpected behavior, we assert it's equal temporarily.
		// this logic may be changed in the future.
		packageName := strings.TrimSpace(cfg.Upstream.Package.Name)
		if packageName != path {
			panic("directory name is not equal to package name in llpkg.cfg")
		}
		d.checkVersion(ver, cfg)

		allPaths = append(allPaths, path)
	}

	// 1. Check there's only one directory in PR
	if len(pathMap) > 1 {
		panic("too many to-be-converted directory")
	}

	// 2. Check config files(llpkg.cfg and llcppg.cfg)
	if len(pathMap) == 0 {
		panic("no valid config files, llpkg.cfg and llcppg.cfg must exist")
	}

	return allPaths
}

// Postprocessing handles version tagging and record updates after PR merge
// Creates Git tags, updates version records, and cleans up legacy branches
func (d *DefaultClient) Postprocessing() {
	// https://docs.github.com/en/actions/writing-workflows/choosing-when-your-workflow-runs/events-that-trigger-workflows#push
	sha := LatestCommitSHA()
	// check it's associated with a pr
	if !d.isAssociatedWithPullRequest(sha) {
		// not a merge commit, skip it.
		panic("not a merge request commit")
	}

	version := d.mappedVersion()
	// skip it when no mapped version is found
	if version == "" {
		panic("no mapped version found in the commit message")
	}

	if hasTag(version) {
		panic("tag has already existed")
	}

	if err := d.createTag(version, sha); err != nil {
		panic(err)
	}

	// create a release
	d.createReleaseByTag(version)

	clib, mappedVersion := parseMappedVersion(version)

	// the pr has merged, so we can read it.
	cfg, err := config.ParseLLPkgConfig(filepath.Join(clib, "llpkg.cfg"))
	must(err)

	// write it to llpkgstore.json
	ver := versions.Read("llpkgstore.json")
	ver.Write(clib, cfg.Upstream.Package.Version, mappedVersion)

	// we have finished tagging the commit, safe to remove the branch
	if branchName, isLegacy := d.isLegacyVersion(); isLegacy {
		d.removeBranch(branchName)
	}
	// move to website in Github Action...
}

func (d *DefaultClient) Release() {
	version := d.mappedVersion()
	// skip it when no mapped version is found
	if version == "" {
		panic("no mapped version found in the commit message")
	}

	clib, _ := parseMappedVersion(version)
	// the pr has merged, so we can read it.
	cfg, err := config.ParseLLPkgConfig(filepath.Join(clib, "llpkg.cfg"))
	must(err)

	uc, err := config.NewUpstreamFromConfig(cfg.Upstream)
	must(err)

	tempDir, _ := os.MkdirTemp("", "llpkg-tool")
	_, err = uc.Installer.Install(uc.Pkg, tempDir)
	must(err)

	pkgConfigDir := filepath.Join(tempDir, "lib", "pkgconfig")
	// clear exist .pc
	os.RemoveAll(pkgConfigDir)

	err = os.Mkdir(pkgConfigDir, 0777)
	must(err)
	pcFiles := filepath.Join(tempDir, "*.pc")

	matches, _ := filepath.Glob(pcFiles)

	if len(matches) == 0 {
		panic("no pc file found, this should not happen")
	}
	// generate pc template to lib/pkgconfig
	for _, matchPC := range matches {
		err := pc.GenerateTemplateFromPC(matchPC, pkgConfigDir)
		must(err)
		// okay, safe to remove old pc
		os.Remove(matchPC)
	}

	file.RemovePattern(filepath.Join(tempDir, "*.sh"))

	zipFilePath, _ := filepath.Abs(binaryZip(uc.Pkg.Name))

	err = file.Zip(tempDir, zipFilePath)
	must(err)

	release := d.getReleaseByTag(version)

	// upload file to release
	err = d.uploadFileToRelease(zipFilePath, release)
	must(err)

}

// CreateBranchFromLabel creates release branch based on label format
// Follows naming convention: release-branch.<CLibraryName>/<MappedVersion>
func (d *DefaultClient) CreateBranchFromLabel(labelName string) {
	// design: branch:release-branch.{CLibraryName}/{MappedVersion}
	branchName := strings.TrimPrefix(strings.TrimSpace(labelName), LabelPrefix)
	if branchName == labelName {
		panic("invalid label name format")
	}

	// fast-path: branch exists, can skip.
	if d.hasBranch(branchName) {
		return
	}
	version := strings.TrimPrefix(branchName, BranchPrefix)
	if version == branchName {
		panic("invalid label name format")
	}
	clib, _ := parseMappedVersion(version)
	// slow-path: check the condition if we can create a branch
	//
	// create a branch only when this version is legacy.
	// according to branch maintenance strategy

	// get latest version of the clib
	ver := versions.Read("llpkgstore.json")

	cversions := ver.CVersions(clib)
	if len(cversions) == 0 {
		panic("no clib found")
	}

	if !versions.IsSemver(cversions) {
		panic("c version dones't follow semver, skip maintaining.")
	}

	err := d.createBranch(branchName, shaFromTag(version))
	must(err)
}

// CleanResource removes labels and resources after issue resolution
// Verifies issue closure via PR merge before deletion
func (d *DefaultClient) CleanResource() {
	issueEvent := IssueEvent()

	issueNumber := int(issueEvent["number"].(float64))
	regex := regexp.MustCompile(fmt.Sprintf(`(f|F)ix.*#%d`, issueNumber))

	// 1. check this issue is closed by a PR
	// In Github, close a issue with a commit whose message follows this format
	// fix/Fix* #{IssueNumber}
	found := false
	for _, commit := range d.allCommits() {
		message := commit.Commit.GetMessage()

		if regex.MatchString(message) &&
			d.isAssociatedWithPullRequest(commit.GetSHA()) {
			found = true
			break
		}
	}

	if !found {
		panic("current issue isn't closed by merged PR.")
	}

	var labelName string

	// 2. find out the branch name from the label
	for _, labels := range issueEvent["labels"].([]map[string]any) {
		label := labels["name"].(string)

		if strings.HasPrefix(label, BranchPrefix) {
			labelName = label
			break
		}
	}

	if labelName == "" {
		panic("current issue hasn't labelled, this should not happen")
	}

	d.removeLabel(labelName)
}
