/**
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package downloader

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	log "github.com/Sirupsen/logrus"

	"mynewt.apache.org/newt/newt/settings"
	"mynewt.apache.org/newt/util"
)

type DownloaderCommitType int

const (
	COMMIT_TYPE_REMOTE_BRANCH DownloaderCommitType = iota
	COMMIT_TYPE_LOCAL_BRANCH
	COMMIT_TYPE_TAG
	COMMIT_TYPE_HASH
)

type Downloader interface {
	FetchFile(path string, filename string, dstDir string) error
	GetCommit() string
	SetCommit(commit string)
	DownloadRepo(commit string, dstPath string) error
	HashFor(path string, commit string) (string, error)
	CommitsFor(path string, commit string) ([]string, error)
	UpdateRepo(path string, branchName string) error
	AreChanges(path string) (bool, error)
	CommitType(path string, commit string) (DownloaderCommitType, error)
	FixupOrigin(path string) error
}

type GenericDownloader struct {
	commit string

	// Whether 'origin' has been fetched during this run.
	fetched bool
}

type GithubDownloader struct {
	GenericDownloader
	Server string
	User   string
	Repo   string

	// Login for private repos.
	Login string

	// Password for private repos.
	Password string

	// Name of environment variable containing the password for private repos.
	// Only used if the Password field is empty.
	PasswordEnv string
}

type GitDownloader struct {
	GenericDownloader
	Url string
}

type LocalDownloader struct {
	GenericDownloader

	// Path to parent directory of repository.yml file.
	Path string
}

func gitPath() (string, error) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		return "", util.NewNewtError(fmt.Sprintf("Can't find git binary: %s\n",
			err.Error()))
	}

	return filepath.ToSlash(gitPath), nil
}

func executeGitCommand(dir string, cmd []string, logCmd bool) ([]byte, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, util.NewNewtError(err.Error())
	}

	gp, err := gitPath()
	if err != nil {
		return nil, err
	}

	if err := os.Chdir(dir); err != nil {
		return nil, util.ChildNewtError(err)
	}

	defer os.Chdir(wd)

	gitCmd := []string{gp}
	gitCmd = append(gitCmd, cmd...)
	output, err := util.ShellCommandLimitDbgOutput(gitCmd, nil, logCmd, -1)
	if err != nil {
		return nil, err
	}

	return output, nil
}

func commitExists(repoDir string, commit string) bool {
	cmd := []string{
		"show-ref",
		"--verify",
		"--quiet",
		"refs/heads/" + commit,
	}
	_, err := executeGitCommand(repoDir, cmd, true)
	return err == nil
}

func initSubmodules(path string) error {
	cmd := []string{
		"submodule",
		"init",
	}

	_, err := executeGitCommand(path, cmd, true)
	if err != nil {
		return err
	}

	return nil
}

func updateSubmodules(path string) error {
	cmd := []string{
		"submodule",
		"update",
	}

	_, err := executeGitCommand(path, cmd, true)
	if err != nil {
		return err
	}

	return nil
}

// checkout does checkout a branch, or create a new branch from a tag name
// if the commit supplied is a tag. sha1 based commits have no special
// handling and result in dettached from HEAD state.
func checkout(repoDir string, commit string) error {
	var cmd []string
	ct, err := commitType(repoDir, commit)
	if err != nil {
		return err
	}

	full, err := fullCommitName(repoDir, commit)
	if err != nil {
		return err
	}

	if ct == COMMIT_TYPE_TAG {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "Will create new branch %s"+
			" from %s\n", commit, full)
		cmd = []string{
			"checkout",
			full,
			"-b",
			commit,
		}
	} else {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "Will checkout %s\n", full)
		cmd = []string{
			"checkout",
			commit,
		}
	}
	if _, err := executeGitCommand(repoDir, cmd, true); err != nil {
		return err
	}

	// Always initialize and update submodules on checkout.  This prevents the
	// repo from being in a modified "(new commits)" state immediately after
	// switching commits.  If the submodules have already been updated, this
	// does not generate any network activity.
	if err := initSubmodules(repoDir); err != nil {
		return err
	}
	if err := updateSubmodules(repoDir); err != nil {
		return err
	}

	return nil
}

// mergees applies upstream changes to the local copy and must be
// preceeded by a "fetch" to achieve any meaningful result.
func merge(repoDir string, commit string) error {
	if err := checkout(repoDir, commit); err != nil {
		return err
	}

	ct, err := commitType(repoDir, commit)
	if err != nil {
		return err
	}

	// We want to merge the remote version of this branch.
	if ct == COMMIT_TYPE_LOCAL_BRANCH {
		ct = COMMIT_TYPE_REMOTE_BRANCH
	}

	full, err := prependCommitPrefix(commit, ct)
	if err != nil {
		return err
	}

	cmd := []string{
		"merge",
		"--no-commit",
		"--no-ff",
		full}
	if _, err := executeGitCommand(repoDir, cmd, true); err != nil {
		util.StatusMessage(util.VERBOSITY_VERBOSE,
			"Merging changes from %s: %s\n", full, err)
		return err
	}

	util.StatusMessage(util.VERBOSITY_VERBOSE,
		"Merging changes from %s\n", full)
	return nil
}

func mergeBase(repoDir string, commit string) (string, error) {
	cmd := []string{
		"merge-base",
		commit,
		commit,
	}
	o, err := executeGitCommand(repoDir, cmd, true)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(o)), nil
}

func branchExists(repoDir string, branchName string) bool {
	cmd := []string{
		"show-ref",
		"--verify",
		"--quiet",
		"refs/heads/" + branchName,
	}
	_, err := executeGitCommand(repoDir, cmd, true)
	return err == nil
}

func commitType(repoDir string, commit string) (DownloaderCommitType, error) {
	if commit == "HEAD" {
		return COMMIT_TYPE_HASH, nil
	}

	if _, err := mergeBase(repoDir, commit); err == nil {
		// Distinguish local branch from hash.
		if branchExists(repoDir, commit) {
			return COMMIT_TYPE_LOCAL_BRANCH, nil
		} else {
			return COMMIT_TYPE_HASH, nil
		}
	}

	if _, err := mergeBase(repoDir, "origin/"+commit); err == nil {
		return COMMIT_TYPE_REMOTE_BRANCH, nil
	}
	if _, err := mergeBase(repoDir, "tags/"+commit); err == nil {
		return COMMIT_TYPE_TAG, nil
	}

	return DownloaderCommitType(-1), util.FmtNewtError(
		"Cannot determine commit type of \"%s\"", commit)
}

func areChanges(repoDir string) (bool, error) {
	cmd := []string{
		"diff",
		"--name-only",
	}

	o, err := executeGitCommand(repoDir, cmd, true)
	if err != nil {
		return false, err
	}

	return len(o) > 0, nil
}

func prependCommitPrefix(commit string, ct DownloaderCommitType) (string, error) {
	switch ct {
	case COMMIT_TYPE_REMOTE_BRANCH:
		return "origin/" + commit, nil
	case COMMIT_TYPE_TAG:
		return "tags/" + commit, nil
	case COMMIT_TYPE_HASH, COMMIT_TYPE_LOCAL_BRANCH:
		return commit, nil
	default:
		return "", util.FmtNewtError("unknown commit type: %d", int(ct))
	}
}

func fullCommitName(path string, commit string) (string, error) {
	ct, err := commitType(path, commit)
	if err != nil {
		return "", err
	}

	return prependCommitPrefix(commit, ct)
}

func showFile(
	path string, branch string, filename string, dstDir string) error {

	if err := os.MkdirAll(dstDir, os.ModePerm); err != nil {
		return util.ChildNewtError(err)
	}

	full, err := fullCommitName(path, branch)
	if err != nil {
		return err
	}

	cmd := []string{
		"show",
		fmt.Sprintf("%s:%s", full, filename),
	}

	dstPath := fmt.Sprintf("%s/%s", dstDir, filename)
	log.Debugf("Fetching file %s to %s", filename, dstPath)
	data, err := executeGitCommand(path, cmd, true)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(dstPath, data, os.ModePerm); err != nil {
		return util.ChildNewtError(err)
	}

	return nil
}

func getRemoteUrl(path string, remote string) (string, error) {
	cmd := []string{
		"remote",
		"get-url",
		remote,
	}

	o, err := executeGitCommand(path, cmd, true)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(o)), nil
}

func setRemoteUrlCmd(remote string, url string) []string {
	return []string{
		"remote",
		"set-url",
		remote,
		url,
	}
}

func setRemoteUrl(path string, remote string, url string, logCmd bool) error {
	cmd := setRemoteUrlCmd(remote, url)
	_, err := executeGitCommand(path, cmd, logCmd)
	return err
}

func warnWrongOriginUrl(path string, curUrl string, goodUrl string) {
	util.StatusMessage(util.VERBOSITY_QUIET,
		"WARNING: Repo's \"origin\" remote points to unexpected URL: "+
			"%s; correcting it to %s.  Repo contents may be incorrect.\n",
		curUrl, goodUrl)
}

func (gd *GenericDownloader) GetCommit() string {
	return gd.commit
}

func (gd *GenericDownloader) SetCommit(branch string) {
	gd.commit = branch
}

func (gd *GenericDownloader) CommitType(
	path string, commit string) (DownloaderCommitType, error) {

	return commitType(path, commit)
}

func (gd *GenericDownloader) HashFor(path string, commit string) (string, error) {
	full, err := fullCommitName(path, commit)
	if err != nil {
		return "", err
	}
	cmd := []string{"rev-parse", full}
	o, err := executeGitCommand(path, cmd, true)
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(string(o)), nil
}

func (gd *GenericDownloader) CommitsFor(
	path string, commit string) ([]string, error) {

	// Hash.
	hash, err := gd.HashFor(path, commit)
	if err != nil {
		return nil, err
	}

	// Branches and tags.
	cmd := []string{
		"for-each-ref",
		"--format=%(refname:short)",
		"--points-at",
		hash,
	}
	o, err := executeGitCommand(path, cmd, true)
	if err != nil {
		return nil, err
	}

	lines := []string{hash}
	text := strings.TrimSpace(string(o))
	if text != "" {
		lines = append(lines, strings.Split(text, "\n")...)
	}

	sort.Strings(lines)
	return lines, nil
}

// Fetches the downloader's origin remote if it hasn't been fetched yet during
// this run.
func (gd *GenericDownloader) cachedFetch(fn func() error) error {
	if gd.fetched {
		return nil
	}

	if err := fn(); err != nil {
		return err
	}

	gd.fetched = true
	return nil
}

func (gd *GithubDownloader) fetch(repoDir string) error {
	return gd.cachedFetch(func() error {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "Fetching repo %s\n",
			gd.Repo)

		_, err := gd.authenticatedCommand(repoDir, []string{"fetch", "--tags"})
		return err
	})
}

func (gd *GithubDownloader) password() string {
	if gd.Password != "" {
		return gd.Password
	} else if gd.PasswordEnv != "" {
		return os.Getenv(gd.PasswordEnv)
	} else {
		return ""
	}
}

func (gd *GithubDownloader) authenticatedCommand(path string,
	args []string) ([]byte, error) {

	if err := gd.setRemoteAuth(path); err != nil {
		return nil, err
	}
	defer gd.clearRemoteAuth(path)

	return executeGitCommand(path, args, true)
}

func (gd *GithubDownloader) FetchFile(
	path string, filename string, dstDir string) error {

	if err := gd.fetch(path); err != nil {
		return err
	}

	if err := showFile(path, gd.GetCommit(), filename, dstDir); err != nil {
		return err
	}

	return nil
}

func (gd *GithubDownloader) UpdateRepo(path string, branchName string) error {
	err := gd.fetch(path)
	if err != nil {
		return err
	}

	// Ignore error, probably resulting from a branch not available at origin
	// anymore.
	merge(path, branchName)

	if err := checkout(path, branchName); err != nil {
		return err
	}

	return nil
}

func (gd *GithubDownloader) AreChanges(path string) (bool, error) {
	return areChanges(path)
}

func (gd *GithubDownloader) remoteUrls() (string, string) {
	server := "github.com"

	if gd.Server != "" {
		server = gd.Server
	}

	var auth string
	if gd.Login != "" {
		pw := gd.password()
		auth = fmt.Sprintf("%s:%s@", gd.Login, pw)
	}

	url := fmt.Sprintf("https://%s%s/%s/%s.git", auth, server, gd.User,
		gd.Repo)
	publicUrl := fmt.Sprintf("https://%s/%s/%s.git", server, gd.User, gd.Repo)

	return url, publicUrl
}

func (gd *GithubDownloader) setOriginUrl(path string, url string) error {
	// Hide password in logged command.
	safeUrl := url
	pw := gd.password()
	if pw != "" {
		safeUrl = strings.Replace(safeUrl, pw, "<password-hidden>", -1)
	}
	util.LogShellCmd(setRemoteUrlCmd("origin", safeUrl), nil)

	return setRemoteUrl(path, "origin", url, false)
}

func (gd *GithubDownloader) clearRemoteAuth(path string) error {
	url, publicUrl := gd.remoteUrls()
	if url == publicUrl {
		return nil
	}

	return gd.setOriginUrl(path, publicUrl)
}

func (gd *GithubDownloader) setRemoteAuth(path string) error {
	url, publicUrl := gd.remoteUrls()
	if url == publicUrl {
		return nil
	}

	return gd.setOriginUrl(path, url)
}

func (gd *GithubDownloader) DownloadRepo(commit string, dstPath string) error {
	// Currently only the master branch is supported.
	branch := "master"

	url, publicUrl := gd.remoteUrls()

	util.StatusMessage(util.VERBOSITY_DEFAULT,
		"Downloading repository %s (commit: %s) from %s\n",
		gd.Repo, commit, publicUrl)

	gp, err := gitPath()
	if err != nil {
		return err
	}

	// Clone the repository.
	cmd := []string{
		gp,
		"clone",
		"-b",
		branch,
		url,
		dstPath,
	}

	if util.Verbosity >= util.VERBOSITY_VERBOSE {
		err = util.ShellInteractiveCommand(cmd, nil)
	} else {
		_, err = util.ShellCommand(cmd, nil)
	}
	if err != nil {
		return err
	}

	defer gd.clearRemoteAuth(dstPath)

	// Checkout the specified commit.
	if err := checkout(dstPath, commit); err != nil {
		return err
	}

	return nil
}

func (gd *GithubDownloader) FixupOrigin(path string) error {
	curUrl, err := getRemoteUrl(path, "origin")
	if err != nil {
		return err
	}

	// Use the public URL, i.e., hide the login and password.
	_, publicUrl := gd.remoteUrls()
	if curUrl == publicUrl {
		return nil
	}

	warnWrongOriginUrl(path, curUrl, publicUrl)
	return gd.setOriginUrl(path, publicUrl)
}

func NewGithubDownloader() *GithubDownloader {
	return &GithubDownloader{}
}

func (gd *GitDownloader) fetch(repoDir string) error {
	return gd.cachedFetch(func() error {
		util.StatusMessage(util.VERBOSITY_VERBOSE, "Fetching repo %s\n",
			gd.Url)
		_, err := executeGitCommand(repoDir, []string{"fetch", "--tags"}, true)
		return err
	})
}

func (gd *GitDownloader) FetchFile(
	path string, filename string, dstDir string) error {

	if err := gd.fetch(path); err != nil {
		return err
	}

	if err := showFile(path, gd.GetCommit(), filename, dstDir); err != nil {
		return err
	}

	return nil
}

func (gd *GitDownloader) UpdateRepo(path string, branchName string) error {
	err := gd.fetch(path)
	if err != nil {
		return err
	}

	// Ignore error, probably resulting from a branch not available at origin
	// anymore.
	merge(path, branchName)

	if err := checkout(path, branchName); err != nil {
		return err
	}

	return nil
}

func (gd *GitDownloader) AreChanges(path string) (bool, error) {
	return areChanges(path)
}

func (gd *GitDownloader) DownloadRepo(commit string, dstPath string) error {
	// Currently only the master branch is supported.
	branch := "master"

	util.StatusMessage(util.VERBOSITY_DEFAULT,
		"Downloading repository %s (commit: %s)\n", gd.Url, commit)

	gp, err := gitPath()
	if err != nil {
		return err
	}

	// Clone the repository.
	cmd := []string{
		gp,
		"clone",
		"-b",
		branch,
		gd.Url,
		dstPath,
	}

	if util.Verbosity >= util.VERBOSITY_VERBOSE {
		err = util.ShellInteractiveCommand(cmd, nil)
	} else {
		_, err = util.ShellCommand(cmd, nil)
	}
	if err != nil {
		return err
	}

	// Checkout the specified commit.
	if err := checkout(dstPath, commit); err != nil {
		return err
	}

	return nil
}

func (gd *GitDownloader) FixupOrigin(path string) error {
	curUrl, err := getRemoteUrl(path, "origin")
	if err != nil {
		return err
	}

	if curUrl == gd.Url {
		return nil
	}

	warnWrongOriginUrl(path, curUrl, gd.Url)
	return setRemoteUrl(path, "origin", gd.Url, true)
}

func NewGitDownloader() *GitDownloader {
	return &GitDownloader{}
}

func (ld *LocalDownloader) FetchFile(
	path string, filename string, dstDir string) error {

	srcPath := ld.Path + "/" + filename
	dstPath := dstDir + "/" + filename

	log.Debugf("Fetching file %s to %s", srcPath, dstPath)
	if err := util.CopyFile(srcPath, dstPath); err != nil {
		return err
	}

	return nil
}

func (ld *LocalDownloader) UpdateRepo(path string, branchName string) error {
	os.RemoveAll(path)
	return ld.DownloadRepo(branchName, path)
}

func (ld *LocalDownloader) AreChanges(path string) (bool, error) {
	return areChanges(path)
}

func (ld *LocalDownloader) DownloadRepo(commit string, dstPath string) error {
	util.StatusMessage(util.VERBOSITY_DEFAULT,
		"Downloading local repository %s\n", ld.Path)

	if err := util.CopyDir(ld.Path, dstPath); err != nil {
		return err
	}

	// Checkout the specified commit.
	if err := checkout(dstPath, commit); err != nil {
		return err
	}

	return nil
}

func (ld *LocalDownloader) FixupOrigin(path string) error {
	return nil
}

func NewLocalDownloader() *LocalDownloader {
	return &LocalDownloader{}
}

func loadError(format string, args ...interface{}) error {
	return util.NewNewtError(
		"error loading project.yml: " + fmt.Sprintf(format, args...))
}

func LoadDownloader(repoName string, repoVars map[string]string) (
	Downloader, error) {

	switch repoVars["type"] {
	case "github":
		gd := NewGithubDownloader()

		gd.Server = repoVars["server"]
		gd.User = repoVars["user"]
		gd.Repo = repoVars["repo"]

		// The project.yml file can contain github access tokens and
		// authentication credentials, but this file is probably world-readable
		// and therefore not a great place for this.
		gd.Login = repoVars["login"]
		gd.Password = repoVars["password"]
		gd.PasswordEnv = repoVars["password_env"]

		// Alternatively, the user can put security material in
		// $HOME/.newt/repos.yml.
		newtrc := settings.Newtrc()
		privRepo := newtrc.GetValStringMapString("repository."+repoName, nil)
		if privRepo != nil {
			if gd.Login == "" {
				gd.Login = privRepo["login"]
			}
			if gd.Password == "" {
				gd.Password = privRepo["password"]
			}
			if gd.PasswordEnv == "" {
				gd.PasswordEnv = privRepo["password_env"]
			}
		}
		return gd, nil

	case "git":
		gd := NewGitDownloader()
		gd.Url = repoVars["url"]
		if gd.Url == "" {
			return nil, loadError("repo \"%s\" missing required field \"url\"",
				repoName)
		}
		return gd, nil

	case "local":
		ld := NewLocalDownloader()
		ld.Path = repoVars["path"]
		return ld, nil

	default:
		return nil, loadError("invalid repository type: %s", repoVars["type"])
	}
}
