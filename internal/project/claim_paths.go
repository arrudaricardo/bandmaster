package project

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type pathSemantics struct {
	caseFold             bool
	unicodeNormalization bool
	probeDevice          uint64
}

func validateClaimPath(claimPath string) *Error {
	if !utf8.ValidString(claimPath) || claimPath == "" || filepath.IsAbs(claimPath) || strings.Contains(claimPath, `\`) || path.Clean(claimPath) != claimPath {
		return invalid("invalid_claim_path", fmt.Sprintf("Claim path %q must be a canonical UTF-8 repository-relative path using slash separators.", claimPath))
	}
	parts := strings.Split(claimPath, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return invalid("invalid_claim_path", fmt.Sprintf("Claim path %q contains an invalid segment.", claimPath))
		}
	}
	if parts[0] == ".git" {
		return invalid("invalid_claim_path", "Git metadata paths cannot be claimed.")
	}
	return nil
}
func (p *Project) rejectIgnoredUntrackedPath(claimPath string, indexPaths []string) *Error {
	for _, indexPath := range indexPaths {
		if indexPath == claimPath {
			return nil
		}
	}
	ignored, err := gitQuiet(p.Root, "check-ignore", "--quiet", "--no-index", "--", claimPath)
	if err != nil {
		return internal("inspect untracked path ignore policy", err)
	}
	if ignored {
		return invalid("ignored_untracked_path", fmt.Sprintf("Ignored untracked path %s stays outside Bandmaster ownership and rollback guarantees.", claimPath))
	}
	return nil
}

func (p *Project) claimPathSemantics() (pathSemantics, *Error) {
	probeDevice, err := filesystemDevice(p.GitDir)
	if err != nil {
		return pathSemantics{}, internal("inspect filesystem probe device", err)
	}
	suffixBytes := make([]byte, 8)
	if _, err := rand.Read(suffixBytes); err != nil {
		return pathSemantics{}, internal("generate filesystem probe identity", err)
	}
	suffix := hex.EncodeToString(suffixBytes)
	caseFold, err := filesystemNamesAlias(p.GitDir, ".bandmaster-case-"+suffix+"-a", ".bandmaster-case-"+suffix+"-A")
	if err != nil {
		return pathSemantics{}, internal("detect filesystem case folding", err)
	}
	normalizes, err := filesystemNamesAlias(p.GitDir, ".bandmaster-unicode-"+suffix+"-\u00e9", ".bandmaster-unicode-"+suffix+"-e\u0301")
	if err != nil {
		return pathSemantics{}, internal("detect filesystem Unicode normalization", err)
	}
	return pathSemantics{caseFold: caseFold, unicodeNormalization: normalizes, probeDevice: probeDevice}, nil
}

func filesystemDevice(filePath string) (uint64, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("filesystem device identity is unavailable")
	}
	return uint64(stat.Dev), nil
}

func filesystemNamesAlias(directory, storedName, alternateName string) (bool, error) {
	storedPath := filepath.Join(directory, storedName)
	file, err := os.OpenFile(storedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(storedPath)
		return false, err
	}
	defer os.Remove(storedPath)
	storedInfo, err := os.Lstat(storedPath)
	if err != nil {
		return false, err
	}
	alternateInfo, err := os.Lstat(filepath.Join(directory, alternateName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(storedInfo, alternateInfo), nil
}

func validateClaimAliases(paths []string, semantics pathSemantics) *Error {
	for index, claimPath := range paths {
		for _, part := range strings.Split(claimPath, "/") {
			if semantics.componentIdentity(part) == semantics.componentIdentity(".git") {
				return invalid("invalid_claim_path", "Git metadata paths cannot be claimed.")
			}
		}
		for _, existing := range paths[:index] {
			if claimPathsConflict(claimPath, existing, semantics) {
				return invalid("alias_claim_path", fmt.Sprintf("Claim paths %s and %s alias or nest under the worktree filesystem's path rules.", existing, claimPath))
			}
		}
	}
	return nil
}

func claimPathsConflict(left, right string, semantics pathSemantics) bool {
	leftParts := strings.Split(left, "/")
	rightParts := strings.Split(right, "/")
	common := len(leftParts)
	if len(rightParts) < common {
		common = len(rightParts)
	}
	for index := 0; index < common; index++ {
		if semantics.componentIdentity(leftParts[index]) != semantics.componentIdentity(rightParts[index]) {
			return false
		}
	}
	return true
}

func (s pathSemantics) componentIdentity(value string) string {
	if s.unicodeNormalization {
		value = norm.NFC.String(value)
	}
	if s.caseFold {
		value = cases.Fold().String(value)
	}
	return value
}

func (p *Project) gitIndexPaths() ([]string, *Error) {
	output, err := gitBytes(p.Root, "ls-files", "-z")
	if err != nil {
		return nil, internal("inspect Git index paths", err)
	}
	if len(output) == 0 {
		return nil, nil
	}
	if output[len(output)-1] != 0 {
		return nil, internal("parse Git index paths", errors.New("missing NUL terminator"))
	}
	records := strings.Split(string(output[:len(output)-1]), "\x00")
	return records, nil
}

func (p *Project) validateClaimPathSpelling(claimPath string, indexPaths []string, semantics pathSemantics) *Error {
	identityMatches := make([]string, 0, 1)
	for _, indexPath := range indexPaths {
		if claimPathsConflict(claimPath, indexPath, semantics) && len(strings.Split(claimPath, "/")) == len(strings.Split(indexPath, "/")) {
			identityMatches = append(identityMatches, indexPath)
		}
	}
	if len(identityMatches) > 1 {
		return invalid("ambiguous_claim_path", fmt.Sprintf("Claim path %s aliases multiple paths in the Git index.", claimPath))
	}
	if len(identityMatches) == 1 && identityMatches[0] != claimPath {
		return invalid("noncanonical_claim_path", fmt.Sprintf("Claim path %s must use Git-index spelling %s.", claimPath, identityMatches[0]))
	}

	current := p.Root
	parts := strings.Split(claimPath, "/")
	for index, part := range parts {
		entries, err := os.ReadDir(current)
		if err != nil {
			return internal("inspect claim path spelling", err)
		}
		exact := false
		for _, entry := range entries {
			if entry.Name() == part {
				exact = true
				break
			}
		}
		next := filepath.Join(current, filepath.FromSlash(part))
		info, statErr := os.Lstat(next)
		if errors.Is(statErr, os.ErrNotExist) {
			device, err := filesystemDevice(current)
			if err != nil {
				return internal("inspect absent claim destination filesystem", err)
			}
			if device != semantics.probeDevice {
				return invalid("ambiguous_claim_path", fmt.Sprintf("Absent claim path %s is on a filesystem whose alias behavior cannot be resolved safely.", claimPath))
			}
			localSemantics, err := directoryPathSemantics(current, semantics)
			if err != nil {
				return internal("inspect absent claim destination path semantics", err)
			}
			if localSemantics.caseFold != semantics.caseFold || localSemantics.unicodeNormalization != semantics.unicodeNormalization {
				return invalid("ambiguous_claim_path", fmt.Sprintf("Absent claim path %s is beneath a directory with different alias behavior and cannot be resolved safely.", claimPath))
			}
			return nil
		}
		if statErr != nil {
			return internal("inspect claim path spelling", statErr)
		}
		if existingPathAliasesGitMetadata(current, next, info) {
			return invalid("invalid_claim_path", "Git metadata paths cannot be claimed.")
		}
		if !exact {
			prefix := strings.Join(parts[:index+1], "/")
			if !indexHasExactPrefix(indexPaths, prefix) {
				return invalid("noncanonical_claim_path", fmt.Sprintf("Claim path %s does not use the existing directory-entry spelling for %s.", claimPath, prefix))
			}
		}
		if index < len(parts)-1 && info.Mode()&os.ModeSymlink != 0 {
			return invalid("unsupported_claim_path", fmt.Sprintf("Claim path %s traverses a parent symlink.", claimPath))
		}
		current = next
	}
	return nil
}

func existingPathAliasesGitMetadata(parent, candidate string, candidateInfo os.FileInfo) bool {
	metadataInfo, err := os.Lstat(filepath.Join(parent, ".git"))
	return err == nil && os.SameFile(candidateInfo, metadataInfo) && filepath.Base(candidate) != ".git"
}

func indexHasExactPrefix(indexPaths []string, prefix string) bool {
	for _, indexPath := range indexPaths {
		if indexPath == prefix || strings.HasPrefix(indexPath, prefix+"/") {
			return true
		}
	}
	return false
}
func (p *Project) changedPaths() ([]string, *Error) {
	output, err := gitBytes(p.Root, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, internal("inspect changed paths", err)
	}
	var changed []string
	for len(output) > 0 {
		end := strings.IndexByte(string(output), 0)
		if end < 0 {
			return nil, internal("parse changed paths", errors.New("missing NUL terminator"))
		}
		record := string(output[:end])
		output = output[end+1:]
		if len(record) < 4 {
			return nil, internal("parse changed paths", errors.New("short status record"))
		}
		changed = append(changed, record[3:])
	}
	return changed, nil
}
