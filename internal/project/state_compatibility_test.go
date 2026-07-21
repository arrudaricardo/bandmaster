package project

import "testing"

func TestRecoveryTransitionsProduceCompatibleRetryStates(t *testing.T) {
	tests := []struct {
		previousBatch string
		wantSession   string
		wantBatch     string
	}{
		{previousBatch: "collecting", wantSession: "active", wantBatch: "collecting"},
		{previousBatch: "repair_pending", wantSession: "active", wantBatch: "repair_pending"},
		{previousBatch: "repairing", wantSession: "active", wantBatch: "repairing"},
		{previousBatch: "frozen", wantSession: "finalizing", wantBatch: "frozen"},
		{previousBatch: "validating", wantSession: "finalizing", wantBatch: "frozen"},
		{previousBatch: "finalizing", wantSession: "finalizing", wantBatch: "finalizing"},
		{previousBatch: "final_validating", wantSession: "finalizing", wantBatch: "finalizing"},
	}
	for _, test := range tests {
		t.Run(test.previousBatch, func(t *testing.T) {
			sessionStatus, batchStatus, ok := recoveryTransition(test.previousBatch)
			if !ok || sessionStatus != test.wantSession || batchStatus != test.wantBatch {
				t.Fatalf("recovery transition for %s = (%s, %s, %t), want (%s, %s, true)", test.previousBatch, sessionStatus, batchStatus, ok, test.wantSession, test.wantBatch)
			}
		})
	}
}

func TestCompatibilityTableDocumentsEveryPersistedState(t *testing.T) {
	wantSessions := []string{"active", "paused", "finalizing", "completed", "aborting", "aborted"}
	wantBatches := []string{"collecting", "frozen", "validating", "repair_pending", "repairing", "finalizing", "final_validating", "committed", "quarantined", "abandoned"}
	for _, status := range wantSessions {
		if !tableDocumentsSession(status) {
			t.Errorf("session state %q is absent from compatibility table", status)
		}
	}
	for _, status := range wantBatches {
		if !tableDocumentsBatch(status) {
			t.Errorf("batch state %q is absent from compatibility table", status)
		}
	}
}

func tableDocumentsSession(status string) bool {
	for _, transition := range sessionBatchTransitions {
		if transition.sessionStatus == status {
			return true
		}
	}
	return false
}

func tableDocumentsBatch(status string) bool {
	for _, transition := range sessionBatchTransitions {
		if transition.batchStatus == status {
			return true
		}
	}
	return false
}
