package project

import "fmt"

type Error struct {
	Code            string
	Message         string
	Retryable       bool
	ExitCode        int
	SessionID       string
	InitiatingError *ErrorDetail
	RollbackError   *RollbackErrorDetail
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type RollbackErrorDetail struct {
	Operation string `json:"operation"`
	Message   string `json:"message"`
}

type Project struct {
	Root   string
	GitDir string
}

type InitResult struct {
	ConfigPath       string `json:"config_path"`
	SkillPath        string `json:"skill_path"`
	ValidationDigest string `json:"validation_digest"`
	Approved         bool   `json:"approved"`
}

type ConfigStatus struct {
	ValidationDigest string `json:"validation_digest"`
	Approved         bool   `json:"approved"`
}

func invalid(code, message string) *Error {
	return &Error{Code: code, Message: message, ExitCode: 3}
}

func blocked(sessionID, code, message string) *Error {
	return &Error{Code: code, Message: message, Retryable: true, ExitCode: 2, SessionID: sessionID}
}

func quarantined(sessionID, code, message string) *Error {
	return &Error{Code: code, Message: message, ExitCode: 4, SessionID: sessionID}
}

func internal(action string, err error) *Error {
	return &Error{Code: "internal_error", Message: fmt.Sprintf("%s: %v", action, err), ExitCode: 1}
}
