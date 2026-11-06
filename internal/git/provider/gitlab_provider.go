package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/driver/gitlab"
	"github.com/jenkins-x/go-scm/scm/factory"
	"github.com/jenkins-x/go-scm/scm/transport/oauth2"
)

type GitLabProvider struct {
	log         logr.Logger
	apiToken    string
	hostname    string
	oauthConfig *GitLabOAuthConfig
	client      *scm.Client
}

func (p *GitLabProvider) ListPullRequestChanges(ctx context.Context, pr PullRequest) ([]Change, error) {
	changes := []Change{}

	changeList, _, err := p.client.PullRequests.ListChanges(ctx, pr.Repository.String(), pr.Number, &scm.ListOptions{})
	if err != nil {
		return changes, fmt.Errorf("unable to list merge request changes: %w", err)
	}

	for _, change := range changeList {
		changes = append(changes, Change{
			Path:         change.Path,
			PreviousPath: change.PreviousPath,
			Patch:        change.Patch,
			Sha:          change.Sha,
			Additions:    change.Additions,
			Deletions:    change.Deletions,
			Changes:      change.Changes,
			Added:        change.Added,
			Renamed:      change.Renamed,
			Deleted:      change.Deleted,
		})
	}

	return changes, nil
}

func (p *GitLabProvider) ListPullRequests(ctx context.Context, repo Repository) ([]PullRequest, error) {
	var prs []PullRequest

	opts := scm.PullRequestListOptions{Page: 1, Size: defaultPageSize, Open: true}
	for {
		prList, res, err := p.client.PullRequests.List(ctx, repo.String(), &opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list merge requests: %w", err)
		}

		for _, pr := range prList {
			prs = append(prs, PullRequest{
				Repository: repo,
				Number:     pr.Number,
				BaseBranch: pr.Base.Ref,
				HeadBranch: pr.Head.Ref,
				BaseSha:    pr.Base.Sha,
				HeadSha:    pr.Head.Sha,
				Closed:     pr.Closed,
			})
		}

		if res.Page.Next == 0 || opts.Page >= maxPages {
			break
		}
		opts.Page = res.Page.Next
	}

	return prs, nil
}

func (p *GitLabProvider) AddCommentToPullRequest(ctx context.Context, pr PullRequest, body []byte) (*Comment, error) {
	return p.notesRequest(ctx, http.MethodPost, pr, 0, body)
}

func (p *GitLabProvider) GetLastComments(ctx context.Context, pr PullRequest, since time.Time) ([]*Comment, error) {
	var allComments []*scm.Comment

	opts := scm.ListOptions{Page: 1, Size: defaultPageSize}
	for {
		comments, res, err := p.client.PullRequests.ListComments(ctx, pr.Repository.String(), pr.Number, &opts)
		if err != nil {
			return nil, fmt.Errorf("failed to list merge request comments: %w", err)
		}

		allComments = append(allComments, comments...)

		if res.Page.Next == 0 || opts.Page >= maxPages {
			break
		}
		opts.Page = res.Page.Next
	}

	if len(allComments) == 0 {
		return nil, nil
	}

	sort.Slice(allComments, func(i, j int) bool {
		return allComments[i].Created.After(allComments[j].Created)
	})

	var commentsSince []*Comment
	for _, comment := range allComments {
		if comment.Created.After(since) {
			commentsSince = append(commentsSince, &Comment{
				ID:   comment.ID,
				Link: comment.Link,
				Body: comment.Body,
			})
		}
	}

	return commentsSince, nil
}

func (p *GitLabProvider) UpdateCommentOfPullRequest(ctx context.Context, pr PullRequest, commentID int, body []byte) error {
	note, found, err := p.getNote(ctx, pr, commentID)
	if err != nil {
		return err
	}

	// note deleted manually or already carries a plan — post a new one
	if !found || strings.Contains(note.Body, "```hcl") {
		_, err := p.notesRequest(ctx, http.MethodPost, pr, 0, body)

		return err
	}

	_, err = p.notesRequest(ctx, http.MethodPut, pr, commentID, body)

	return err
}

// getNote returns an MR note by ID. The second value reports whether it
// exists: go-scm's GitLab driver does not map 404 to scm.ErrNotFound (only
// the GitHub driver does), so the former errors.Is(scm.ErrNotFound) branch
// was dead and a manually deleted note blocked plan comments forever.
func (p *GitLabProvider) getNote(ctx context.Context, pr PullRequest, commentID int) (note gitlabNote, found bool, err error) {
	httpClient, err := p.notesClient()
	if err != nil {
		return note, false, err
	}

	endpoint := fmt.Sprintf("https://%s/api/v4/projects/%s/merge_requests/%d/notes/%d",
		p.hostname, url.PathEscape(pr.Repository.String()), pr.Number, commentID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return note, false, fmt.Errorf("failed to build notes request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return note, false, fmt.Errorf("failed to get comment from merge request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, resp.Body)

		return note, false, nil
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_, _ = io.Copy(io.Discard, resp.Body)

		return note, false, fmt.Errorf("failed to get comment from merge request: %s: %s", resp.Status, string(respBody))
	}

	if err := json.NewDecoder(resp.Body).Decode(&note); err != nil {
		return note, false, fmt.Errorf("failed to decode notes response: %w", err)
	}

	return note, true, nil
}

type gitlabNote struct {
	ID   int    `json:"id"`
	Body string `json:"body"`
}

// notesPhrase returns the verb phrase for error messages:
// "add comment to" / "update comment in".
func notesPhrase(method string) string {
	if method == http.MethodPut {
		return "update comment in"
	}

	return "add comment to"
}

// notesClient makes sure the provider went through Setup(): notesRequest and
// getNote bypass go-scm and must reuse the client's transport chain
// (PrivateToken or OAuth2) instead of an unauthenticated default client.
func (p *GitLabProvider) notesClient() (*http.Client, error) {
	if p.client == nil || p.client.Client == nil {
		return nil, errors.New("gitlab provider is not set up: call Setup() first")
	}

	return p.client.Client, nil
}

// notesRequest creates (commentID == 0) or updates an MR note.
// It bypasses go-scm: the driver passes the note text as a query parameter
// (?body=...), and a full plan (tens of KB percent-encoded) breaks the
// request — GitLab answers HTTP/2 GOAWAY ENHANCE_YOUR_CALM (oversized
// request line). The note body goes as a JSON request body instead
// (GitLab's limit is 1M characters, orders of magnitude above any plan).
// Authentication (PrivateToken/OAuth2) already lives in the transport
// chain of p.client.Client.
func (p *GitLabProvider) notesRequest(ctx context.Context, method string, pr PullRequest, commentID int, body []byte) (*Comment, error) {
	httpClient, err := p.notesClient()
	if err != nil {
		return nil, err
	}

	endpoint := fmt.Sprintf("https://%s/api/v4/projects/%s/merge_requests/%d/notes",
		p.hostname, url.PathEscape(pr.Repository.String()), pr.Number)
	if commentID != 0 {
		endpoint += "/" + strconv.Itoa(commentID)
	}

	payload, err := json.Marshal(map[string]string{"body": string(body)})
	if err != nil {
		return nil, fmt.Errorf("failed to encode comment body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("failed to build notes request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to %s merge request: %w", notesPhrase(method), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_, _ = io.Copy(io.Discard, resp.Body)

		return nil, fmt.Errorf("failed to %s merge request: %s: %s", notesPhrase(method), resp.Status, string(respBody))
	}

	note := struct {
		ID int `json:"id"`
	}{}
	if err := json.NewDecoder(resp.Body).Decode(&note); err != nil {
		return nil, fmt.Errorf("failed to decode notes response: %w", err)
	}

	return &Comment{ID: note.ID}, nil
}

func (p *GitLabProvider) SetLogger(log logr.Logger) error {
	p.log = log

	return nil
}

func (p *GitLabProvider) SetToken(tokenType, token string) error {
	switch tokenType {
	case APITokenType:
		p.apiToken = token
	default:
		return fmt.Errorf("unknown token type: %s", tokenType)
	}

	return nil
}

func (p *GitLabProvider) SetHostname(hostname string) error {
	p.hostname = hostname

	return nil
}

func (p *GitLabProvider) Setup() error {
	if p.hostname == "" {
		p.hostname = "gitlab.com"
	}

	serverURL := fmt.Sprintf("https://%s", p.hostname)

	// OAuth2 authentication: uses a refresh token to obtain and
	// automatically renew access tokens before they expire.
	if p.oauthConfig != nil {
		var err error
		p.client, err = gitlab.New(serverURL)
		if err != nil {
			return fmt.Errorf("failed to create gitlab client: %w", err)
		}

		refresher := &oauth2.Refresher{
			ClientID:     p.oauthConfig.ClientID,
			ClientSecret: p.oauthConfig.ClientSecret,
			Endpoint:     serverURL + "/oauth/token",
			Source: oauth2.StaticTokenSource(&scm.Token{
				Token:   p.oauthConfig.Token,
				Refresh: p.oauthConfig.RefreshToken,
			}),
		}

		p.client.Client = &http.Client{
			Transport: &oauth2.Transport{
				Scheme: "Bearer",
				Source: refresher,
			},
		}

		return nil
	}

	// Static token authentication (PAT, Project/Group Access Token).
	if p.apiToken == "" {
		return fmt.Errorf("missing required option: Token or GitLabOAuth config")
	}

	var err error
	p.client, err = factory.NewClient("gitlab", serverURL, p.apiToken)
	if err != nil {
		return fmt.Errorf("failed to create gitlab client: %w", err)
	}

	return nil
}

func newGitLabProvider() *GitLabProvider {
	return &GitLabProvider{
		log: logr.Discard(),
	}
}
