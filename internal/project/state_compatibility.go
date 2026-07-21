package project

import (
	"database/sql"
	"errors"
	"fmt"
)

// sessionBatchTransitions is the authoritative state-pair table. Public
// mutations may only start from one of these pairs, and integrity recovery uses
// the recovery aliases to turn interrupted states into a retryable public state.
var sessionBatchTransitions = []struct {
	sessionStatus      string
	batchStatus        string
	nextCommand        string
	recoveryFrom       []string
	restoredBatchState string
}{
	{sessionStatus: "active", batchStatus: "", nextCommand: "task create"},
	{sessionStatus: "active", batchStatus: "collecting", nextCommand: "batch freeze", recoveryFrom: []string{"collecting"}, restoredBatchState: "collecting"},
	{sessionStatus: "active", batchStatus: "repair_pending", nextCommand: "task repair", recoveryFrom: []string{"repair_pending"}, restoredBatchState: "repair_pending"},
	{sessionStatus: "active", batchStatus: "repairing", nextCommand: "batch freeze", recoveryFrom: []string{"repairing"}, restoredBatchState: "repairing"},
	{sessionStatus: "active", batchStatus: "committed", nextCommand: "task create"},
	{sessionStatus: "active", batchStatus: "abandoned", nextCommand: "task create"},

	{sessionStatus: "paused", batchStatus: "", nextCommand: "session resume"},
	{sessionStatus: "paused", batchStatus: "collecting", nextCommand: "session resume"},
	{sessionStatus: "paused", batchStatus: "repair_pending", nextCommand: "session resume"},
	{sessionStatus: "paused", batchStatus: "repairing", nextCommand: "session resume"},
	{sessionStatus: "paused", batchStatus: "committed", nextCommand: "session resume"},
	{sessionStatus: "paused", batchStatus: "abandoned", nextCommand: "session resume or session abort"},
	{sessionStatus: "paused", batchStatus: "quarantined", nextCommand: "integrity recover"},

	{sessionStatus: "finalizing", batchStatus: "frozen", nextCommand: "batch validate", recoveryFrom: []string{"frozen", "validating"}, restoredBatchState: "frozen"},
	{sessionStatus: "finalizing", batchStatus: "validating", nextCommand: "batch validate"},
	{sessionStatus: "finalizing", batchStatus: "finalizing", nextCommand: "batch commit", recoveryFrom: []string{"finalizing", "final_validating"}, restoredBatchState: "finalizing"},
	{sessionStatus: "finalizing", batchStatus: "final_validating", nextCommand: "batch commit"},

	{sessionStatus: "aborting", batchStatus: "", nextCommand: "session abort"},
	{sessionStatus: "aborting", batchStatus: "collecting", nextCommand: "session abort"},
	{sessionStatus: "aborting", batchStatus: "repair_pending", nextCommand: "session abort"},
	{sessionStatus: "aborting", batchStatus: "repairing", nextCommand: "session abort"},
	{sessionStatus: "aborting", batchStatus: "committed", nextCommand: "session abort"},
	{sessionStatus: "aborting", batchStatus: "quarantined", nextCommand: "session abort"},
	{sessionStatus: "aborting", batchStatus: "abandoned", nextCommand: "session abort"},

	{sessionStatus: "completed", batchStatus: "", nextCommand: "session inspect"},
	{sessionStatus: "completed", batchStatus: "committed", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "collecting", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "repair_pending", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "repairing", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "committed", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "quarantined", nextCommand: "session inspect"},
	{sessionStatus: "aborted", batchStatus: "abandoned", nextCommand: "session inspect"},
}

func recoveryTransition(previousBatchStatus string) (sessionStatus, batchStatus string, ok bool) {
	for _, transition := range sessionBatchTransitions {
		for _, previous := range transition.recoveryFrom {
			if previous == previousBatchStatus {
				return transition.sessionStatus, transition.restoredBatchState, true
			}
		}
	}
	return "", "", false
}

func validateSessionBatchPair(queryer rowQuerier, session Session) *Error {
	batchStatus, projectError := latestBatchStatus(queryer, session.ID)
	if projectError != nil {
		return projectError
	}
	for _, transition := range sessionBatchTransitions {
		if transition.sessionStatus == session.Status && transition.batchStatus == batchStatus {
			return nil
		}
	}
	return invalidSession(session.ID, "incompatible_session_batch_state", fmt.Sprintf("Session %s in %s state is incompatible with its latest batch in %s state; explicit integrity recovery is required.", session.ID, session.Status, displayBatchStatus(batchStatus)))
}

func latestBatchStatus(queryer rowQuerier, sessionID string) (string, *Error) {
	var status string
	err := queryer.QueryRow(`SELECT status FROM batches WHERE session_id = ? ORDER BY creation_order DESC LIMIT 1`, sessionID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", sessionInternal(sessionID, "inspect session batch compatibility", err)
	}
	return status, nil
}

func displayBatchStatus(status string) string {
	if status == "" {
		return "none"
	}
	return status
}
