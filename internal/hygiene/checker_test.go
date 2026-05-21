package hygiene

import (
	"context"
	"os"
	"testing"
)

func TestRunner_H001_pass(t *testing.T) {
	f, err := os.CreateTemp("", "README.md")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	// Rename to expected path in current dir
	os.Rename(f.Name(), "README.md")
	defer os.Remove("README.md")

	ctx := context.Background()
	runner := NewRunner("")
	results := runner.Run(ctx, []Check{{ID: "H001", Name: "README", Ticked: true}})
	if len(results) != 1 || !results[0].Passed {
		t.Errorf("expected H001 to pass, got %+v", results[0])
	}
}

func TestRunner_H001_fail(t *testing.T) {
	os.Remove("README.md")
	ctx := context.Background()
	runner := NewRunner("")
	results := runner.Run(ctx, []Check{{ID: "H001", Name: "README", Ticked: true}})
	if len(results) != 1 || results[0].Passed {
		t.Errorf("expected H001 to fail, got %+v", results[0])
	}
}

func TestRunner_H002_pass(t *testing.T) {
	os.MkdirAll(".github", 0755)
	f, _ := os.Create(".github/CODEOWNERS")
	if f != nil {
		f.Close()
	}
	defer os.RemoveAll(".github")

	ctx := context.Background()
	runner := NewRunner("")
	results := runner.Run(ctx, []Check{{ID: "H002", Name: "CODEOWNERS", Ticked: true}})
	if len(results) != 1 || !results[0].Passed {
		t.Errorf("expected H002 to pass, got %+v", results[0])
	}
}

func TestRunner_H006_pass(t *testing.T) {
	os.MkdirAll(".devcontainer", 0755)
	f, _ := os.Create(".devcontainer/devcontainer.json")
	if f != nil {
		f.Close()
	}
	defer os.RemoveAll(".devcontainer")

	ctx := context.Background()
	runner := NewRunner("")
	results := runner.Run(ctx, []Check{{ID: "H006", Name: "Devcontainer", Ticked: true}})
	if len(results) != 1 || !results[0].Passed {
		t.Errorf("expected H006 to pass, got %+v", results[0])
	}
}

func TestRunner_noTickedItems(t *testing.T) {
	ctx := context.Background()
	runner := NewRunner("")
	results := runner.Run(ctx, []Check{
		{ID: "H001", Name: "README", Ticked: false},
	})
	if len(results) != 0 {
		t.Errorf("expected no results for unticked items, got %d", len(results))
	}
}

func TestRunner_H005_pass(t *testing.T) {
	summary := "github.com/foo/bar.go:10:\tFoo\t100.0%\ntotal:\t\t\t(statements)\t97.3%"
	runner := NewRunner(summary)
	results := runner.Run(context.Background(), []Check{{ID: "H005", Name: "Coverage", Ticked: true}})
	if len(results) != 1 || !results[0].Passed {
		t.Errorf("expected H005 to pass, got %+v", results[0])
	}
}

func TestRunner_H005_fail_below_threshold(t *testing.T) {
	summary := "github.com/foo/bar.go:10:\tFoo\t80.0%\ntotal:\t\t\t(statements)\t82.1%"
	runner := NewRunner(summary)
	results := runner.Run(context.Background(), []Check{{ID: "H005", Name: "Coverage", Ticked: true}})
	if len(results) != 1 || results[0].Passed {
		t.Errorf("expected H005 to fail, got %+v", results[0])
	}
}

func TestRunner_H005_fail_no_summary(t *testing.T) {
	runner := NewRunner("")
	results := runner.Run(context.Background(), []Check{{ID: "H005", Name: "Coverage", Ticked: true}})
	if len(results) != 1 || results[0].Passed {
		t.Errorf("expected H005 to fail with no summary, got %+v", results[0])
	}
}

func TestFormatReport(t *testing.T) {
	results := []Result{
		{ID: "H001", Name: "README", Passed: true, Details: "found"},
		{ID: "H002", Name: "CODEOWNERS", Passed: false, Details: "missing"},
	}
	report := FormatReport(results)
	if report == "" {
		t.Error("expected non-empty report")
	}
}
