package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jenkins-x/go-scm/scm/driver/gitlab"
	"github.com/jenkins-x/go-scm/scm/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitLabProviderListPullRequestsRequestsOnlyOpen(t *testing.T) {
	state := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state <- r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, "[]")
		assert.NoError(t, err)
	}))
	t.Cleanup(server.Close)

	client, err := gitlab.New(server.URL)
	require.NoError(t, err)

	p := &GitLabProvider{client: client}
	prs, err := p.ListPullRequests(t.Context(), Repository{Org: "example", Name: "infrastructure"})
	require.NoError(t, err)
	assert.Empty(t, prs)
	assert.Equal(t, "opened", <-state)
}

// newGitLabTestProvider starts a fake GitLab and a provider that went
// through the full Setup(). The client transport is PrivateToken on top of
// the test server's TLS transport: both authentication and self-signed
// trust are exercised.
func newGitLabTestProvider(t *testing.T, handler http.HandlerFunc) (*GitLabProvider, *httptest.Server) {
	t.Helper()

	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	p := newGitLabProvider()
	require.NoError(t, p.SetToken(APITokenType, "test-token"))
	require.NoError(t, p.SetHostname(serverURL.Host))
	require.NoError(t, p.Setup())
	p.client.Client = &http.Client{
		Transport: &transport.PrivateToken{
			Token: "test-token",
			Base:  server.Client().Transport,
		},
	}

	return p, server
}

var testPR = PullRequest{Repository: Repository{Org: "devops", Name: "auto-tf/platform-resources"}, Number: 197}

const testNotesPath = "/api/v4/projects/devops%2Fauto-tf%2Fplatform-resources/merge_requests/197/notes"

// TestGitLabProviderAddCommentSendsLargeBodyAsJSON: the note body must be
// sent as a JSON request body. go-scm passes the text as a query parameter
// (?body=...), and a large plan gets HTTP/2 GOAWAY ENHANCE_YOUR_CALM from
// GitLab (oversized request line) — incident 2026-11-06, MR !197.
func TestGitLabProviderAddCommentSendsLargeBodyAsJSON(t *testing.T) {
	var gotMethod, gotPath, gotContentType, gotToken string
	var gotBody map[string]string
	var decodeErr error

	p, _ := newGitLabTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotContentType = r.Method, r.URL.EscapedPath(), r.Header.Get("Content-Type")
		gotToken = r.Header.Get("Private-Token")
		decodeErr = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":424242, "body":"ok"}`)
		assert.NoError(t, err)
	})

	// body larger than any sane request-line limit (8KB/16KB)
	plan := "tf-controller plan output:\n```hcl\n" + strings.Repeat("~ resource \"warpgate_target\" \"targets\" {\n", 8000) + "```\n"

	comment, err := p.AddCommentToPullRequest(t.Context(), testPR, []byte(plan))
	require.NoError(t, err)
	require.NoError(t, decodeErr)
	assert.Equal(t, 424242, comment.ID)

	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "application/json", gotContentType)
	assert.Equal(t, "test-token", gotToken)
	// the percent-encoded project path must stay intact
	assert.Equal(t, testNotesPath, gotPath)
	assert.Equal(t, plan, gotBody["body"])
	assert.NotContains(t, gotPath, "body=")
}

// TestGitLabProviderUpdateCommentUsesPut: updating an existing note is a
// PUT to /notes/{id} with a JSON body.
func TestGitLabProviderUpdateCommentUsesPut(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string
	var decodeErr error

	p, _ := newGitLabTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		if r.Method != http.MethodGet {
			decodeErr = json.NewDecoder(r.Body).Decode(&gotBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, `{"id":777, "body":"replan"}`)
		assert.NoError(t, err)
	})

	err := p.UpdateCommentOfPullRequest(t.Context(), testPR, 777, []byte("tf-controller plan output (updated)"))
	require.NoError(t, err)
	require.NoError(t, decodeErr)

	assert.Equal(t, http.MethodPut, gotMethod)
	assert.Equal(t, testNotesPath+"/777", gotPath)
	assert.Equal(t, "tf-controller plan output (updated)", gotBody["body"])
}

// TestGitLabProviderUpdateCommentDeletedFallsBackToPost: the note from the
// annotation was deleted by a human (404) — the plan goes into a new note
// and the informer does not get stuck. Before the fix this branch was dead:
// go-scm's GitLab driver never returns scm.ErrNotFound (GitHub's does).
func TestGitLabProviderUpdateCommentDeletedFallsBackToPost(t *testing.T) {
	var posts, gets []string

	p, _ := newGitLabTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			gets = append(gets, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotFound)
			_, err := io.WriteString(w, `{"message":"404 Not Found"}`)
			assert.NoError(t, err)
		case r.Method == http.MethodPost:
			posts = append(posts, r.URL.EscapedPath())
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"id":999, "body":"new"}`)
			assert.NoError(t, err)
		default:
			t.Errorf("unexpected method: %s %s", r.Method, r.URL.EscapedPath())
		}
	})

	err := p.UpdateCommentOfPullRequest(t.Context(), testPR, 777, []byte("tf-controller plan output"))
	require.NoError(t, err)

	require.Len(t, gets, 1)
	assert.Equal(t, testNotesPath+"/777", gets[0])
	require.Len(t, posts, 1)
	assert.Equal(t, testNotesPath, posts[0])
}

// TestGitLabProviderUpdateCommentWithPlanCreatesNew: the existing note
// already carries a plan (```hcl) — it must not be edited; post a new one.
func TestGitLabProviderUpdateCommentWithPlanCreatesNew(t *testing.T) {
	var posts, puts []string

	p, _ := newGitLabTestProvider(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, "{\"id\":777, \"body\":\"old plan\\n```hcl\\n...```\"}")
			assert.NoError(t, err)
		case r.Method == http.MethodPost:
			posts = append(posts, r.URL.EscapedPath())
			w.Header().Set("Content-Type", "application/json")
			_, err := io.WriteString(w, `{"id":998, "body":"new"}`)
			assert.NoError(t, err)
		case r.Method == http.MethodPut:
			puts = append(puts, r.URL.EscapedPath())
			w.WriteHeader(http.StatusTeapot)
		default:
			t.Errorf("unexpected method: %s %s", r.Method, r.URL.EscapedPath())
		}
	})

	err := p.UpdateCommentOfPullRequest(t.Context(), testPR, 777, []byte("tf-controller plan output"))
	require.NoError(t, err)

	assert.Empty(t, puts, "PUT must not be called when the existing note contains ```hcl")
	require.Len(t, posts, 1)
	assert.Equal(t, testNotesPath, posts[0])
}

// TestGitLabProviderNotesRequiresSetup: a provider without Setup() must
// fail loudly instead of sending unauthenticated requests over the network.
func TestGitLabProviderNotesRequiresSetup(t *testing.T) {
	p := newGitLabProvider()

	_, err := p.AddCommentToPullRequest(t.Context(), testPR, []byte("plan"))
	require.ErrorContains(t, err, "not set up")

	err = p.UpdateCommentOfPullRequest(t.Context(), testPR, 1, []byte("plan"))
	require.ErrorContains(t, err, "not set up")
}
