/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lgtm

import (
	"fmt"
	"regexp"

	"github.com/sirupsen/logrus"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
	"k8s.io/test-infra/prow/repoowners"
)

const pluginName = "lgtm"

var (
	lgtmLabel           = "lgtm"
	lgtmRe              = regexp.MustCompile(`(?mi)^/lgtm(?: no-issue)?\s*$`)
	lgtmCancelRe        = regexp.MustCompile(`(?mi)^/lgtm cancel\s*$`)
	removeLGTMLabelNoti = "New changes are detected. LGTM label has been removed."
)

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericComment, helpProvider)
	plugins.RegisterPullRequestHandler(pluginName, func(pc plugins.PluginClient, pe github.PullRequestEvent) error {
		return handlePullRequest(pc.GitHubClient, pe, pc.Logger)
	}, helpProvider)
}

func helpProvider(config *plugins.Configuration, enabledRepos []string) (*pluginhelp.PluginHelp, error) {
	// The Config field is omitted because this plugin is not configurable.
	pluginHelp := &pluginhelp.PluginHelp{
		Description: "The lgtm plugin manages the application and removal of the 'lgtm' (Looks Good To Me) label which is typically used to gate merging.",
	}
	pluginHelp.AddCommand(pluginhelp.Command{
		Usage:       "/lgtm [cancel]",
		Description: "Adds or removes the 'lgtm' label which is typically used to gate merging.",
		Featured:    true,
		WhoCanUse:   "Collaborators on the repository. '/lgtm cancel' can be used additionally by the PR author.",
		Examples:    []string{"/lgtm", "/lgtm cancel"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	IsCollaborator(owner, repo, login string) (bool, error)
	AddLabel(owner, repo string, number int, label string) error
	AssignIssue(owner, repo string, number int, assignees []string) error
	CreateComment(owner, repo string, number int, comment string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
	GetPullRequest(org, repo string, number int) (*github.PullRequest, error)
	GetPullRequestChanges(org, repo string, number int) ([]github.PullRequestChange, error)
	ListIssueComments(org, repo string, number int) ([]github.IssueComment, error)
	DeleteComment(org, repo string, ID int) error
	BotName() (string, error)
}

func handleGenericComment(pc plugins.PluginClient, e github.GenericCommentEvent) error {
	return handle(pc.GitHubClient, pc.PluginConfig, pc.OwnersClient, pc.Logger, &e)
}

func handle(gc githubClient, config *plugins.Configuration, ownersClient repoowners.Interface, log *logrus.Entry, e *github.GenericCommentEvent) error {
	// Only consider open PRs and new comments.
	if !e.IsPR || e.IssueState != "open" || e.Action != github.GenericCommentActionCreated {
		return nil
	}

	// If we create an "/lgtm" comment, add lgtm if necessary.
	// If we create a "/lgtm cancel" comment, remove lgtm if necessary.
	wantLGTM := false
	if lgtmRe.MatchString(e.Body) {
		wantLGTM = true
	} else if lgtmCancelRe.MatchString(e.Body) {
		wantLGTM = false
	} else {
		return nil
	}

	org := e.Repo.Owner.Login
	repo := e.Repo.Name
	commentAuthor := e.User.Login

	// Allow authors to cancel LGTM. Do not allow authors to LGTM, and do not
	// accept commands from any other user.
	isAssignee := false
	for _, assignee := range e.Assignees {
		if assignee.Login == e.User.Login {
			isAssignee = true
			break
		}
	}
	// If we need to skip collaborator checks for this repo, what we actually need
	// to do is skip assignment checks and use OWNERS files to determine whether the
	// commenter can lgtm the PR.
	skipCollaborators := skipCollaborators(config, org, repo)
	isAuthor := e.User.Login == e.IssueAuthor.Login
	if isAuthor && wantLGTM {
		resp := "you cannot LGTM your own PR."
		log.Infof("Commenting with \"%s\".", resp)
		return gc.CreateComment(org, repo, e.Number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, commentAuthor, resp))
	} else if !isAuthor && !isAssignee && !skipCollaborators {
		log.Infof("Assigning %s/%s#%d to %s", org, repo, e.Number, commentAuthor)
		if err := gc.AssignIssue(org, repo, e.Number, []string{commentAuthor}); err != nil {
			msg := "assigning you to the PR failed"
			if ok, merr := gc.IsCollaborator(org, repo, commentAuthor); merr == nil && !ok {
				msg = fmt.Sprintf("only %s/%s repo collaborators may be assigned issues", org, repo)
			} else if merr != nil {
				log.WithError(merr).Errorf("Failed IsCollaborator(%s, %s, %s)", org, repo, commentAuthor)
			} else {
				log.WithError(err).Errorf("Failed AssignIssue(%s, %s, %d, %s)", org, repo, e.Number, commentAuthor)
			}
			resp := "changing LGTM is restricted to assignees, and " + msg + "."
			log.Infof("Reply to assign via /lgtm request with comment: \"%s\"", resp)
			return gc.CreateComment(org, repo, e.Number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, commentAuthor, resp))
		}
	} else if !isAuthor && skipCollaborators {
		log.Debugf("Skipping collaborator checks and loading OWNERS for %s/%s#%d", org, repo, e.Number)
		ro, err := loadRepoOwners(gc, ownersClient, org, repo, e.Number)
		if err != nil {
			return err
		}
		filenames, err := getChangedFiles(gc, org, repo, e.Number)
		if err != nil {
			return err
		}
		if !loadReviewers(ro, filenames).Has(github.NormLogin(commentAuthor)) {
			resp := "adding LGTM is restricted to approvers and reviewers in OWNERS files."
			log.Infof("Reply to /lgtm request with comment: \"%s\"", resp)
			return gc.CreateComment(org, repo, e.Number, plugins.FormatResponseRaw(e.Body, e.HTMLURL, commentAuthor, resp))
		}
	}

	// Only add the label if it doesn't have it, and vice versa.
	hasLGTM := false
	labels, err := gc.GetIssueLabels(org, repo, e.Number)
	if err != nil {
		log.WithError(err).Errorf("Failed to get the labels on %s/%s#%d.", org, repo, e.Number)
	}
	for _, candidate := range labels {
		if candidate.Name == lgtmLabel {
			hasLGTM = true
			break
		}
	}
	if hasLGTM && !wantLGTM {
		log.Info("Removing LGTM label.")
		return gc.RemoveLabel(org, repo, e.Number, lgtmLabel)
	} else if !hasLGTM && wantLGTM {
		log.Info("Adding LGTM label.")
		if err := gc.AddLabel(org, repo, e.Number, lgtmLabel); err != nil {
			return err
		}
		// Delete the LGTM removed noti after the LGTM label is added.
		botname, err := gc.BotName()
		if err != nil {
			log.WithError(err).Errorf("Failed to get bot name.")
		}
		comments, err := gc.ListIssueComments(org, repo, e.Number)
		if err != nil {
			log.WithError(err).Errorf("Failed to get the list of issue comments on %s/%s#%d.", org, repo, e.Number)
		}
		for _, comment := range comments {
			if comment.User.Login == botname && comment.Body == removeLGTMLabelNoti {
				if err := gc.DeleteComment(org, repo, comment.ID); err != nil {
					log.WithError(err).Errorf("Failed to delete comment from %s/%s#%d, ID:%d.", org, repo, e.Number, comment.ID)
				}
			}
		}
	}
	return nil
}

type ghLabelClient interface {
	RemoveLabel(owner, repo string, number int, label string) error
	CreateComment(owner, repo string, number int, comment string) error
}

func handlePullRequest(gc ghLabelClient, pe github.PullRequestEvent, log *logrus.Entry) error {
	if pe.PullRequest.Merged {
		return nil
	}

	if pe.Action != github.PullRequestActionSynchronize {
		return nil
	}

	// Don't bother checking if it has the label...it's a race, and we'll have
	// to handle failure due to not being labeled anyway.
	org := pe.PullRequest.Base.Repo.Owner.Login
	repo := pe.PullRequest.Base.Repo.Name
	number := pe.PullRequest.Number

	var labelNotFound bool
	if err := gc.RemoveLabel(org, repo, number, lgtmLabel); err != nil {
		if _, labelNotFound = err.(*github.LabelNotFound); !labelNotFound {
			return fmt.Errorf("failed removing lgtm label: %v", err)
		}

		// If the error is indeed *github.LabelNotFound, consider it a success.
	}
	// Creates a comment to inform participants that LGTM label is removed due to new
	// pull request changes.
	if !labelNotFound {
		log.Infof("Create a LGTM removed notification to %s/%s#%d  with a message: %s", org, repo, number, removeLGTMLabelNoti)
		return gc.CreateComment(org, repo, number, removeLGTMLabelNoti)
	}
	return nil
}

func skipCollaborators(config *plugins.Configuration, org, repo string) bool {
	full := fmt.Sprintf("%s/%s", org, repo)
	for _, elem := range config.Owners.SkipCollaborators {
		if elem == org || elem == full {
			return true
		}
	}
	return false
}

func loadRepoOwners(gc githubClient, ownersClient repoowners.Interface, org, repo string, number int) (repoowners.RepoOwnerInterface, error) {
	pr, err := gc.GetPullRequest(org, repo, number)
	if err != nil {
		return nil, err
	}
	return ownersClient.LoadRepoOwners(org, repo, pr.Base.Ref)
}

// getChangedFiles returns all the changed files for the provided pull request.
func getChangedFiles(gc githubClient, org, repo string, number int) ([]string, error) {
	changes, err := gc.GetPullRequestChanges(org, repo, number)
	if err != nil {
		return nil, fmt.Errorf("cannot get PR changes for %s/%s#%d", org, repo, number)
	}
	var filenames []string
	for _, change := range changes {
		filenames = append(filenames, change.Filename)
	}
	return filenames, nil
}

// loadReviewers returns all reviewers and approvers from all OWNERS files that
// cover the provided filenames.
func loadReviewers(ro repoowners.RepoOwnerInterface, filenames []string) sets.String {
	reviewers := sets.String{}
	for _, filename := range filenames {
		reviewers = reviewers.Union(ro.Approvers(filename)).Union(ro.Reviewers(filename))
	}
	return reviewers
}
