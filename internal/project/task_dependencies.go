package project

import (
	"database/sql"
	"errors"
	"fmt"
)

func validatePrerequisites(queryer rowQuerier, sessionID, taskID string, prerequisites []string) *Error {
	for _, prerequisite := range prerequisites {
		if prerequisite == taskID {
			return invalid("task_dependency_cycle", "A task cannot depend on itself.")
		}
		var foundSession string
		if err := queryer.QueryRow(`SELECT session_id FROM tasks WHERE id = ?`, prerequisite).Scan(&foundSession); errors.Is(err, sql.ErrNoRows) {
			return invalid("task_not_found", fmt.Sprintf("Prerequisite task %s does not exist.", prerequisite))
		} else if err != nil {
			return internal("validate task prerequisite", err)
		}
		if foundSession != sessionID {
			return invalid("task_not_found", fmt.Sprintf("Prerequisite task %s does not belong to the active session.", prerequisite))
		}
	}
	return nil
}

func validateDependencyCycles(queryer rowQuerier, sessionID, taskID string, prerequisites []string) *Error {
	for _, prerequisite := range prerequisites {
		var cycle int
		err := queryer.QueryRow(`
			WITH RECURSIVE ancestors(id) AS (
				SELECT prerequisite_id FROM task_dependencies WHERE task_id = ?
				UNION
				SELECT dependency.prerequisite_id
				FROM task_dependencies dependency
				JOIN ancestors ON dependency.task_id = ancestors.id
			)
			SELECT 1 FROM ancestors WHERE id = ? LIMIT 1`, prerequisite, taskID).Scan(&cycle)
		if err == nil {
			return invalidSession(sessionID, "task_dependency_cycle", fmt.Sprintf("Making task %s depend on %s would create a dependency cycle.", taskID, prerequisite))
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return sessionInternal(sessionID, "validate task dependency graph", err)
		}
	}
	return nil
}

func taskReadiness(queryer rowQuerier, prerequisites []string) (string, *Error) {
	for _, prerequisite := range prerequisites {
		var status string
		if err := queryer.QueryRow(`SELECT status FROM tasks WHERE id = ?`, prerequisite).Scan(&status); err != nil {
			return "", internal("read prerequisite status", err)
		}
		if status != "committed" && status != "no_op" {
			return "planned", nil
		}
	}
	return "ready", nil
}

func taskPrerequisites(db *sql.Tx, sessionID, id string) ([]string, *Error) {
	rows, err := db.Query(`SELECT prerequisite_id FROM task_dependencies WHERE task_id = ? ORDER BY dependency_order`, id)
	if err != nil {
		return nil, sessionInternal(sessionID, "read task prerequisites", err)
	}
	defer rows.Close()
	var prerequisites []string
	for rows.Next() {
		var prerequisite string
		if err := rows.Scan(&prerequisite); err != nil {
			return nil, sessionInternal(sessionID, "read task prerequisite", err)
		}
		prerequisites = append(prerequisites, prerequisite)
	}
	if err := rows.Err(); err != nil {
		return nil, sessionInternal(sessionID, "read task prerequisites", err)
	}
	return prerequisites, nil
}
