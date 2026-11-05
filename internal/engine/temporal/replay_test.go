//go:build temporal

package temporal

import (
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The histories in testdata were recorded from real runs (TestReplay and
// TestKilledMachine with STARCI_RECORD_HISTORY=1). A change to the workflow
// that would not replay them would strand runs in flight when a pool is
// upgraded: change it with workflow.GetVersion, or record them again if no
// run on the old code can be left.
func TestRecordedHistories(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no recorded histories in testdata")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			r := worker.NewWorkflowReplayer()
			r.RegisterWorkflowWithOptions(runWorkflow, workflow.RegisterOptions{Name: workflowName})
			if err := r.ReplayWorkflowHistoryFromJSONFile(nil, f); err != nil {
				t.Fatal(err)
			}
		})
	}
}
