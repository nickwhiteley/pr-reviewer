package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	t.Setenv("GITHUB_TOKEN", "token")
	c, err := NewClient("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(srv.URL)
	return c
}

func TestNewClient_validation(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token")
	if _, err := NewClient("no-slash"); err == nil {
		t.Error("expected error for repo without owner/name format")
	}
	if _, err := NewClient("owner/"); err == nil {
		t.Error("expected error for empty repo name")
	}
	t.Setenv("GITHUB_TOKEN", "")
	if _, err := NewClient("owner/repo"); err == nil {
		t.Error("expected error for missing GITHUB_TOKEN")
	}
}

func TestListComments_paginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		var comments []Comment
		switch page {
		case "1":
			// A full page signals that more may follow.
			for i := range 100 {
				comments = append(comments, Comment{ID: int64(i), Body: fmt.Sprintf("c%d", i)})
			}
		case "2":
			comments = []Comment{{ID: 100, Body: "last"}}
		}
		json.NewEncoder(w).Encode(comments)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)
	comments, err := c.ListComments(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 101 {
		t.Errorf("expected 101 comments across pages, got %d", len(comments))
	}
}

func TestPostOrUpdate(t *testing.T) {
	var created, updated bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/issues/1/comments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode([]Comment{
				{ID: 10, Body: "existing <!-- marker:foo -->"},
			})
		case "POST":
			created = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(Comment{ID: 99})
		}
	})
	mux.HandleFunc("/repos/owner/repo/issues/comments/10", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			updated = true
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := newTestClient(t, srv)

	// Marker present in an existing comment: update in place.
	if err := c.PostOrUpdate(context.Background(), 1, "<!-- marker:foo -->", "new body"); err != nil {
		t.Fatal(err)
	}
	if !updated || created {
		t.Errorf("expected update without create, got updated=%v created=%v", updated, created)
	}

	// Unknown marker: create a fresh comment.
	updated, created = false, false
	if err := c.PostOrUpdate(context.Background(), 1, "<!-- marker:bar -->", "new body"); err != nil {
		t.Fatal(err)
	}
	if !created || updated {
		t.Errorf("expected create without update, got updated=%v created=%v", updated, created)
	}
}

func TestTruncateBody(t *testing.T) {
	marker := "<!-- wd-auto-review:type=hygiene -->"

	short := "short body " + marker
	if got := truncateBody(short, marker); got != short {
		t.Error("short body should be unchanged")
	}

	long := strings.Repeat("é", 40000) + marker // multi-byte content past the limit
	got := truncateBody(long, marker)
	if len(got) > maxCommentLen {
		t.Errorf("truncated body is %d bytes, exceeds limit %d", len(got), maxCommentLen)
	}
	if !strings.Contains(got, marker) {
		t.Error("marker lost in truncation — comment would duplicate on next run")
	}
	if !strings.HasSuffix(strings.TrimSpace(got), marker) {
		t.Error("marker should be re-appended at the end")
	}
}
