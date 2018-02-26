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

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/pluginhelp"
	"k8s.io/test-infra/prow/plugins"
)

const pluginName = "lgtm"

var (
	lgtmLabel    = "lgtm"
	lgtmRe       = regexp.MustCompile(`(?mi)^/lgtm(?: no-issue)?\s*$`)
	lgtmCancelRe = regexp.MustCompile(`(?mi)^/lgtm cancel\s*$`)
)

func init() {
	plugins.RegisterGenericCommentHandler(pluginName, handleGenericComment, helpProvider)
	plugins.RegisterReviewEventHandler(pluginName, handlePullRequestReview, helpProvider)
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
		WhoCanUse:   "Members of the organization that owns the repository. '/lgtm cancel' can be used additionally by the PR author.",
		Examples:    []string{"/lgtm", "/lgtm cancel"},
	})
	return pluginHelp, nil
}

type githubClient interface {
	IsMember(owner, login string) (bool, error)
	AddLabel(owner, repo string, number int, label string) error
	AssignIssue(owner, repo string, number int, assignees []string) error
	CreateComment(owner, repo string, number int, comment string) error
	RemoveLabel(owner, repo string, number int, label string) error
	GetIssueLabels(org, repo string, number int) ([]github.Label, error)
}

type state struct {
	author 		string
	issueAuthor string
	repo 		github.Repo
	assignees 	[]github.User
	number 		int
	body 		string
	htmlURL 	string
}

func handleGenericComment(pc plugins.PluginClient, e github.GenericCommentEvent) error {
	author := e.User.Login
	issueAuthor := e.IssueAuthor.Login
	repo := e.Repo
	assignees := e.Assignees
	number := e.Number
	body := e.Body
	htmlURL := e.HTMLURL	
	log := pc.Logger
	gc := pc.GitHubClient
	
	// Only consider open PRs and new comments.
	if !e.IsPR || e.IssueState != "open" || e.Action != github.GenericCommentActionCreated {
		return nil
	}

	// If we create an "/lgtm" comment, add lgtm if necessary.
	// If we create a "/lgtm cancel" comment, remove lgtm if necessary.
	wantLGTM := false
	if lgtmRe.MatchString(body) {
		wantLGTM = true
	} else if lgtmCancelRe.MatchString(body) {
		wantLGTM = false
	} else {
		return nil
	}

	// Author cannot LGTM own PR
	isAuthor := author == issueAuthor
	if isAuthor && wantLGTM {
		resp := "you cannot LGTM your own PR."
		log.Infof("Commenting with \"%s\".", resp)
		return gc.CreateComment(repo.Owner.Login, repo.Name, number, plugins.FormatResponseRaw(body, htmlURL, author, resp))
	}

	return handle(
		wantLGTM,
		gc, 
		log,
		&state{
			author, 
			issueAuthor,
			repo,  
			assignees, 
			number, 
			body, 
			htmlURL,
		},
	)
}

func handlePullRequestReview(pc plugins.PluginClient, e github.ReviewEvent) error {
	// author := e.Review.User.Login
	// issueAuthor := e.PullRequest.User.Login
	// repo := e.Repo
	// assignees := e.PullRequest.Assignees
	// number := e.PullRequest.Number
	// body := e.Review.Body
	// htmlURL := e.Review.HTMLURL

	// If we review with Approve, add lgtm if necessary.
	// If we review with Request Changes, remove lgtm if necessary.
	wantLGTM := false
	if e.Review.State == "approve" {
		wantLGTM = true
	} else if e.Review.State == "changes_requested" {
		wantLGTM = false
	} else {
		return nil
	}
	
	return handle(
		wantLGTM,	 
		pc.GitHubClient,
		pc.Logger,
		&state{
			author: 		e.Review.User.Login, 
			issueAuthor: 	e.PullRequest.User.Login,
			repo: 			e.Repo,  
			assignees: 		e.PullRequest.Assignees, 
			number: 		e.PullRequest.Number, 
			body: 			e.Review.Body, 
			htmlURL: 		e.Review.HTMLURL,
		},
	)
}

func handle(wantLGTM bool, gc githubClient, log *logrus.Entry, pr *state) error {
	org := pr.repo.Owner.Login
	repoName := pr.repo.Name

	// Determine if reviewer is already assigned
	isAssignee := false
	for _, assignee := range pr.assignees {
		if assignee.Login == pr.author {
			isAssignee = true
			break
		}
	}

	// Add reviewers as assignee
	if !isAssignee {
		log.Infof("Assigning %s/%s#%d to %s", org, repoName, pr.number, pr.author)
		if err := gc.AssignIssue(org, repoName, pr.number, []string{pr.author}); err != nil {
			msg := "assigning you to the PR failed"
			if ok, merr := gc.IsMember(org, pr.author); merr == nil && !ok {
				msg = fmt.Sprintf("only %s org members may be assigned issues", org)
			} else if merr != nil {
				log.WithError(merr).Errorf("Failed IsMember(%s, %s)", org, pr.author)
			} else {
				log.WithError(err).Errorf("Failed AssignIssue(%s, %s, %d, %s)", org, repoName, pr.number, pr.author)
			}
			resp := "changing LGTM is restricted to assignees, and " + msg + "."
			log.Infof("Reply to assign via /lgtm request with comment: \"%s\"", resp)
			return gc.CreateComment(org, repoName, pr.number, plugins.FormatResponseRaw(pr.body, pr.htmlURL, pr.author, resp))
		}
	}

	// Only add the label if it doesn't have it, and vice versa.
	hasLGTM := false
	labels, err := gc.GetIssueLabels(org, repoName, pr.number)
	if err != nil {
		log.WithError(err).Errorf("Failed to get the labels on %s/%s#%d.", org, repoName, pr.number)
	}
	for _, candidate := range labels {
		if candidate.Name == lgtmLabel {
			hasLGTM = true
			break
		}
	}
	if hasLGTM && !wantLGTM {
		log.Info("Removing LGTM label.")
		return gc.RemoveLabel(org, repoName, pr.number, lgtmLabel)
	} else if !hasLGTM && wantLGTM {
		log.Info("Adding LGTM label.")
		return gc.AddLabel(org, repoName, pr.number, lgtmLabel)
	}
	return nil	
}

