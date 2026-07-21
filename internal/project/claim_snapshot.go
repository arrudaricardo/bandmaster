package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type PathSnapshot struct {
	Presence    string `json:"presence"`
	Type        string `json:"type"`
	ContentHash string `json:"content_hash,omitempty"`
	Executable  bool   `json:"executable"`
}
type capturedSnapshot struct {
	Path string
	PathSnapshot
	content []byte
}

func (p *Project) captureClaims(paths []string, semantics pathSemantics) ([]capturedSnapshot, *Error) {
	indexPaths, projectError := p.gitIndexPaths()
	if projectError != nil {
		return nil, projectError
	}
	claims := make([]capturedSnapshot, 0, len(paths))
	for _, claimPath := range paths {
		if projectError := p.validateClaimPathSpelling(claimPath, indexPaths, semantics); projectError != nil {
			return nil, projectError
		}
		if projectError := p.rejectIgnoredUntrackedPath(claimPath, indexPaths); projectError != nil {
			return nil, projectError
		}
		snapshot, projectError := p.capturePath(claimPath)
		if projectError != nil {
			return nil, projectError
		}
		claims = append(claims, capturedSnapshot{Path: claimPath, PathSnapshot: snapshot.PathSnapshot, content: snapshot.content})
	}
	return claims, nil
}
func (p *Project) capturePath(claimPath string) (capturedSnapshot, *Error) {
	current := p.Root
	parts := strings.Split(claimPath, "/")
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return capturedSnapshot{PathSnapshot: PathSnapshot{Presence: "absent", Type: "absent"}}, nil
		}
		if err != nil {
			return capturedSnapshot{}, internal("inspect claim path", err)
		}
		if index < len(parts)-1 {
			if info.Mode()&os.ModeSymlink != 0 {
				return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s traverses a parent symlink.", claimPath))
			}
			if !info.IsDir() {
				return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s has a parent that is not a directory.", claimPath))
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return capturedSnapshot{}, internal("read claimed symlink", err)
			}
			content := []byte(target)
			digest := sha256.Sum256(content)
			return capturedSnapshot{PathSnapshot: PathSnapshot{Presence: "present", Type: "symlink", ContentHash: "sha256:" + hex.EncodeToString(digest[:])}, content: content}, nil
		}
		if !info.Mode().IsRegular() {
			return capturedSnapshot{}, invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s is not a regular file or symlink.", claimPath))
		}
		content, err := os.ReadFile(current)
		if err != nil {
			return capturedSnapshot{}, internal("read claimed file", err)
		}
		digest := sha256.Sum256(content)
		executable, projectError := p.gitVisibleExecutable(claimPath, info.Mode().Perm()&0o100 != 0)
		if projectError != nil {
			return capturedSnapshot{}, projectError
		}
		return capturedSnapshot{PathSnapshot: PathSnapshot{Presence: "present", Type: "regular_file", ContentHash: "sha256:" + hex.EncodeToString(digest[:]), Executable: executable}, content: content}, nil
	}
	return capturedSnapshot{}, invalid("invalid_claim_path", "Claim path must not be empty.")
}

func (p *Project) gitVisibleExecutable(claimPath string, filesystemExecutable bool) (bool, *Error) {
	fileMode, err := gitOutput(p.Root, "config", "--bool", "core.fileMode")
	if err != nil || fileMode != "false" {
		return filesystemExecutable, nil
	}
	output, err := gitBytes(p.Root, "--literal-pathspecs", "ls-files", "--stage", "-z", "--", claimPath)
	if err != nil {
		return false, internal("inspect tracked executable mode", err)
	}
	if len(output) == 0 {
		return filesystemExecutable, nil
	}
	recordEnd := bytes.IndexByte(output, 0)
	if recordEnd < 0 {
		return false, internal("parse tracked executable mode", errors.New("missing NUL terminator"))
	}
	record := output[:recordEnd]
	tab := bytes.IndexByte(record, '\t')
	if tab < 0 || string(record[tab+1:]) != claimPath || tab < 6 {
		return false, internal("parse tracked executable mode", errors.New("unexpected index record"))
	}
	return string(record[:6]) == "100755", nil
}

func publicClaims(snapshots []capturedSnapshot) []Claim {
	claims := make([]Claim, 0, len(snapshots))
	for _, snapshot := range snapshots {
		claims = append(claims, Claim{Path: snapshot.Path, Baseline: snapshot.PathSnapshot})
	}
	return claims
}
