// Package github provides PR comment CRUD via the GitHub REST API.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client wraps the GitHub REST API for PR comments.
type Client struct {
	owner  string
	repo   string
	client *http.Client
	token  string
}

// NewClient creates a GitHub client for the given repository.
func NewClient(repo string) *Client {
	parts := strings.SplitN(repo, "/", 2)
	token := os.Getenv("GITHUB_TOKEN")
	return &Client{
		owner:  parts[0],
		repo:   parts[1],
		client: &http.Client{Timeout: 30 * time.Second},
		token:  token,
	}
}

// Comment represents a GitHub issue comment.
type Comment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

// ListComments returns all comments on a PR.
func (c *Client) ListComments(ctx context.Context, pr int) ([]Comment, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", c.owner, c.repo, pr)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list comments: HTTP %d", resp.StatusCode)
	}

	var comments []Comment
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return nil, fmt.Errorf("decode comments: %w", err)
	}
	return comments, nil
}

// CreateComment posts a new comment.
func (c *Client) CreateComment(ctx context.Context, pr int, body string) (int64, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", c.owner, c.repo, pr)
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
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/comments/%d", c.owner, c.repo, commentID)
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

// PostOrUpdate finds a comment with the given marker, updating it if found or creating a new one.
func (c *Client) PostOrUpdate(ctx context.Context, pr int, marker string, body string) error {
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
