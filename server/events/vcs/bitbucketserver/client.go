package bitbucketserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/runatlantis/atlantis/server/events/vcs/common"

	"github.com/pkg/errors"
	"github.com/runatlantis/atlantis/server/events/models"
	"gopkg.in/go-playground/validator.v9"
)

// maxCommentLength is the maximum number of chars allowed by Bitbucket in a
// single comment.
const maxCommentLength = 32768

type Client struct {
	HttpClient  *http.Client
	Username    string
	Password    string
	BaseURL     string
	AtlantisURL string
}

// NewClient builds a bitbucket cloud client. Returns an error if the baseURL is
// malformed. httpClient is the client to use to make the requests, username
// and password are used as basic auth in the requests, baseURL is the API's
// baseURL, ex. https://corp.com:7990. Don't include the API version, ex.
// '/1.0' since that changes based on the API call. atlantisURL is the
// URL for Atlantis that will be linked to from the build status icons. This
// linking is annoying because we don't have anywhere good to link but a URL is
// required.
func NewClient(httpClient *http.Client, username string, password string, baseURL string, atlantisURL string) (*Client, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	// Remove the trailing '/' from the URL.
	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		return nil, errors.Wrapf(err, "parsing %s", baseURL)
	}
	if parsedURL.Scheme == "" {
		return nil, fmt.Errorf("must have 'http://' or 'https://' in base url %q", baseURL)
	}
	urlWithoutPath := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	return &Client{
		HttpClient:  httpClient,
		Username:    username,
		Password:    password,
		BaseURL:     urlWithoutPath,
		AtlantisURL: atlantisURL,
	}, nil
}

// GetModifiedFiles returns the names of files that were modified in the merge request.
// The names include the path to the file from the repo root, ex. parent/child/file.txt.
func (b *Client) GetModifiedFiles(repo models.Repo, pull models.PullRequest) ([]string, error) {
	var files []string

	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return nil, err
	}
	nextPageStart := 0
	baseURL := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/changes",
		b.BaseURL, projectKey, repo.Name, pull.Num)
	// We'll only loop 1000 times as a safety measure.
	maxLoops := 1000
	for i := 0; i < maxLoops; i++ {
		resp, err := b.makeRequest("GET", fmt.Sprintf("%s?start=%d", baseURL, nextPageStart), nil)
		if err != nil {
			return nil, err
		}
		var changes Changes
		if err := json.Unmarshal(resp, &changes); err != nil {
			return nil, errors.Wrapf(err, "Could not parse response %q", string(resp))
		}
		if err := validator.New().Struct(changes); err != nil {
			return nil, errors.Wrapf(err, "API response %q was missing fields", string(resp))
		}
		for _, v := range changes.Values {
			files = append(files, *v.Path.ToString)
		}
		if *changes.IsLastPage {
			break
		}
		nextPageStart = *changes.NextPageStart
	}

	// Now ensure all files are unique.
	hash := make(map[string]bool)
	var unique []string
	for _, f := range files {
		if !hash[f] {
			unique = append(unique, f)
			hash[f] = true
		}
	}
	return unique, nil
}

func (b *Client) GetProjectKey(repoName string, cloneURL string) (string, error) {
	// Get the project key out of the repo clone URL.
	// Given http://bitbucket.corp:7990/scm/at/atlantis-example.git
	// we want to get 'at'.
	expr := fmt.Sprintf(".*/(.*?)/%s\\.git", repoName)
	capture, err := regexp.Compile(expr)
	if err != nil {
		return "", errors.Wrapf(err, "constructing regex from %q", expr)
	}
	matches := capture.FindStringSubmatch(cloneURL)
	if len(matches) != 2 {
		return "", fmt.Errorf("could not extract project key from %q, regex returned %q", cloneURL, strings.Join(matches, ","))
	}
	return matches[1], nil
}

// CreateComment creates a comment on the merge request. It will write multiple
// comments if a single comment is too long.
func (b *Client) CreateComment(repo models.Repo, pullNum int, comment string) error {
	sepEnd := "\n```\n**Warning**: Output length greater than max comment size. Continued in next comment."
	sepStart := "Continued from previous comment.\n```diff\n"
	comments := common.SplitComment(comment, maxCommentLength, sepEnd, sepStart)
	for _, c := range comments {
		if err := b.postComment(repo, pullNum, c); err != nil {
			return err
		}
	}
	return nil
}

// postComment actually posts the comment. It's a helper for CreateComment().
func (b *Client) postComment(repo models.Repo, pullNum int, comment string) error {
	bodyBytes, err := json.Marshal(map[string]string{"text": comment})
	if err != nil {
		return errors.Wrap(err, "json encoding")
	}
	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/comments", b.BaseURL, projectKey, repo.Name, pullNum)
	_, err = b.makeRequest("POST", path, bytes.NewBuffer(bodyBytes))
	return err
}

// PullIsApproved returns true if the merge request was approved.
func (b *Client) PullIsApproved(repo models.Repo, pull models.PullRequest) (bool, error) {
	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d", b.BaseURL, projectKey, repo.Name, pull.Num)
	resp, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return false, err
	}
	var pullResp PullRequest
	if err := json.Unmarshal(resp, &pullResp); err != nil {
		return false, errors.Wrapf(err, "Could not parse response %q", string(resp))
	}
	if err := validator.New().Struct(pullResp); err != nil {
		return false, errors.Wrapf(err, "API response %q was missing fields", string(resp))
	}
	for _, reviewer := range pullResp.Reviewers {
		if *reviewer.Approved {
			return true, nil
		}
	}
	return false, nil
}

// PullIsMergeable returns true if the merge request has no conflicts and can be merged.
func (b *Client) PullIsMergeable(repo models.Repo, pull models.PullRequest) (bool, error) {
	projectKey, err := b.GetProjectKey(repo.Name, repo.SanitizedCloneURL)
	if err != nil {
		return false, err
	}
	path := fmt.Sprintf("%s/rest/api/1.0/projects/%s/repos/%s/pull-requests/%d/merge", b.BaseURL, projectKey, repo.Name, pull.Num)
	resp, err := b.makeRequest("GET", path, nil)
	if err != nil {
		return false, err
	}
	var mergeStatus MergeStatus
	if err := json.Unmarshal(resp, &mergeStatus); err != nil {
		return false, errors.Wrapf(err, "Could not parse response %q", string(resp))
	}
	if err := validator.New().Struct(mergeStatus); err != nil {
		return false, errors.Wrapf(err, "API response %q was missing fields", string(resp))
	}
	if *mergeStatus.CanMerge && !*mergeStatus.Conflicted {
		return true, nil
	}
	return false, nil
}

// UpdateStatus updates the status of a commit.
func (b *Client) UpdateStatus(repo models.Repo, pull models.PullRequest, status models.CommitStatus, description string) error {
	bbState := "FAILED"
	switch status {
	case models.PendingCommitStatus:
		bbState = "INPROGRESS"
	case models.SuccessCommitStatus:
		bbState = "SUCCESSFUL"
	case models.FailedCommitStatus:
		bbState = "FAILED"
	}

	bodyBytes, err := json.Marshal(map[string]string{
		"key":         "atlantis",
		"url":         b.AtlantisURL,
		"state":       bbState,
		"description": description,
	})

	path := fmt.Sprintf("%s/rest/build-status/1.0/commits/%s", b.BaseURL, pull.HeadCommit)
	if err != nil {
		return errors.Wrap(err, "json encoding")
	}
	_, err = b.makeRequest("POST", path, bytes.NewBuffer(bodyBytes))
	return err
}

// prepRequest adds the HTTP basic auth.
func (b *Client) prepRequest(method string, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(b.Username, b.Password)
	return req, nil
}

func (b *Client) makeRequest(method string, path string, reqBody io.Reader) ([]byte, error) {
	req, err := b.prepRequest(method, path, reqBody)
	if err != nil {
		return nil, errors.Wrap(err, "constructing request")
	}
	if reqBody != nil {
		req.Header.Add("Content-Type", "application/json")
	}
	resp, err := b.HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() // nolint: errcheck
	requestStr := fmt.Sprintf("%s %s", method, path)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated && resp.StatusCode != 204 {
		respBody, _ := ioutil.ReadAll(resp.Body)
		return nil, fmt.Errorf("making request %q unexpected status code: %d, body: %s", requestStr, resp.StatusCode, string(respBody))
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrapf(err, "reading response from request %q", requestStr)
	}
	return respBody, nil
}
