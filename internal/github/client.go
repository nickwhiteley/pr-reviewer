// Package github provides PR comment CRUD via the GitHub REST API.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

// Client wraps the GitHub REST API for PR comments.
type Client struct {
	owner   string
	repo    string
	client  *http.Client
	token   string
	baseURL string
}

// SetBaseURL overrides the API base URL (for testing).
func (c *Client) SetBaseURL(url string) {
	c.baseURL = url
}

// NewClient creates a GitHub client for the given repository.
// It fails fast on a malformed repo or a missing GITHUB_TOKEN so the
// problem surfaces before any AI spend, not as 401s after it.
func NewClient(repo string) (*Client, error) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("repo must be in owner/name format, got %q", repo)
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN is not set")
	}
	return &Client{
		owner:   parts[0],
		repo:    parts[1],
		client:  &http.Client{Timeout: 30 * time.Second},
		token:   token,
		baseURL: "https://api.github.com",
	}, nil
}

// Comment represents a GitHub issue comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ListComments returns all comments on a PR, following pagination.
// Without this, busy PRs (>30 comments on one page) made PostOrUpdate
// miss its marker and post duplicates.
func (c *Client) ListComments(ctx context.Context, pr int) ([]Comment, error) {
	var all []Comment
	for page := 1; ; page++ {
		url := fmt.Sprintf(c.baseURL+"/repos/%s/%s/issues/%d/comments?per_page=100&page=%d",
			c.owner, c.repo, pr, page)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+c.token)
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list comments: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("list comments: HTTP %d", resp.StatusCode)
		}

		var comments []Comment
		err = json.NewDecoder(resp.Body).Decode(&comments)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode comments: %w", err)
		}
		all = append(all, comments...)
		if len(comments) < 100 {
			return all, nil
		}
	}
}

// CreateComment posts a new comment.
func (c *Client) CreateComment(ctx context.Context, pr int, body string) (int64, error) {
	url := fmt.Sprintf(c.baseURL+"/repos/%s/%s/issues/%d/comments", c.owner, c.repo, pr)
	payload := map[string]string{"body": body}
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("create comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return 0, fmt.Errorf("create comment: HTTP %d", resp.StatusCode)
	}

	var result Comment
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode created comment: %w", err)
	}
	return result.ID, nil
}

// UpdateComment edits an existing comment.
func (c *Client) UpdateComment(ctx context.Context, commentID int64, body string) error {
	url := fmt.Sprintf(c.baseURL+"/repos/%s/%s/issues/comments/%d", c.owner, c.repo, commentID)
	payload := map[string]string{"body": body}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("update comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("update comment: HTTP %d", resp.StatusCode)
	}
	return nil
}

// maxCommentLen is GitHub's hard limit on issue comment bodies. Posting a
// longer body fails with HTTP 422 and the review would be lost entirely.
const maxCommentLen = 65536

// truncateBody caps body at maxCommentLen while guaranteeing the idempotency
// marker survives, so a truncated comment can still be found and updated on
// the next run.
func truncateBody(body, marker string) string {
	if len(body) <= maxCommentLen {
		return body
	}
	suffix := "\n\n---\n\n⚠️ **Report truncated** — it exceeded GitHub's comment size limit.\n\n" + marker + "\n"
	cut := maxCommentLen - len(suffix)
	// Avoid splitting a multi-byte rune at the cut point.
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut] + suffix
}

// PostOrUpdate finds a comment with the given marker, updating it if found or creating a new one.
func (c *Client) PostOrUpdate(ctx context.Context, pr int, marker string, body string) error {
	body = truncateBody(body, marker)

	comments, err := c.ListComments(ctx, pr)
	if err != nil {
		return fmt.Errorf("list comments for update: %w", err)
	}

	for _, cm := range comments {
		if strings.Contains(cm.Body, marker) {
			return c.UpdateComment(ctx, cm.ID, body)
		}
	}

	_, err = c.CreateComment(ctx, pr, body)
	return err
}

// ReviewComment is an inline comment on a PR diff.
type ReviewComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	Path string `json:"path"`
	Line int    `json:"line"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
}

// ListReviewComments returns all inline review comments on a PR, following
// pagination. Used to keep inline findings idempotent across re-runs.
func (c *Client) ListReviewComments(ctx context.Context, pr int) ([]ReviewComment, error) {
	var all []ReviewComment
	for page := 1; ; page++ {
		url := fmt.Sprintf(c.baseURL+"/repos/%s/%s/pulls/%d/comments?per_page=100&page=%d",
			c.owner, c.repo, pr, page)
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+c.token)
		req.Header.Set("Accept", "application/vnd.github.v3+json")

		resp, err := c.client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list review comments: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("list review comments: HTTP %d", resp.StatusCode)
		}

		var comments []ReviewComment
		err = json.NewDecoder(resp.Body).Decode(&comments)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("decode review comments: %w", err)
		}
		all = append(all, comments...)
		if len(comments) < 100 {
			return all, nil
		}
	}
}

// CreateReviewComment posts an inline comment on the new ("RIGHT") side of a
// PR diff. commitID must be the PR head SHA and path/line must fall within
// the diff, or GitHub rejects the request with 422.
func (c *Client) CreateReviewComment(ctx context.Context, pr int, commitID, path string, line int, body string) error {
	url := fmt.Sprintf(c.baseURL+"/repos/%s/%s/pulls/%d/comments", c.owner, c.repo, pr)
	payload := map[string]any{
		"body":      body,
		"commit_id": commitID,
		"path":      path,
		"line":      line,
		"side":      "RIGHT",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("create review comment: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create review comment: HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// PullRequest is the subset of PR metadata used to give agents the author's
// stated intent.
type PullRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// GetPR fetches the pull request title and description.
func (c *Client) GetPR(ctx context.Context, pr int) (PullRequest, error) {
	url := fmt.Sprintf(c.baseURL+"/repos/%s/%s/pulls/%d", c.owner, c.repo, pr)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return PullRequest{}, err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := c.client.Do(req)
	if err != nil {
		return PullRequest{}, fmt.Errorf("get pr: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return PullRequest{}, fmt.Errorf("get pr: HTTP %d", resp.StatusCode)
	}

	var result PullRequest
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return PullRequest{}, fmt.Errorf("decode pr: %w", err)
	}
	return result, nil
}
