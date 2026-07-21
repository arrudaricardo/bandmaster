package project

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func Open(cwd string) (*Project, *Error) {
	bare, bareErr := gitOutput(cwd, "rev-parse", "--is-bare-repository")
	if bareErr == nil && bare == "true" {
		return nil, invalid("unsupported_bare_repository", "Bare Git repositories are not supported.")
	}
	root, err := gitOutput(cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, invalid("not_git_repository", "Bandmaster must be run in a Git working tree.")
	}
	gitDir, err := gitOutput(cwd, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return nil, internal("resolve Git metadata directory", err)
	}
	commonDir, err := gitOutput(cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, internal("resolve common Git metadata directory", err)
	}
	gitDir = filepath.Clean(gitDir)
	commonDir = filepath.Clean(commonDir)
	if gitDir != commonDir {
		return nil, invalid("unsupported_linked_worktree", "Linked Git worktrees are not supported.")
	}
	worktrees, err := gitOutput(cwd, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, internal("inspect linked Git worktrees", err)
	}
	if strings.Count("\n"+worktrees, "\nworktree ") > 1 {
		return nil, invalid("unsupported_linked_worktree", "Repositories with linked Git worktrees are not supported.")
	}
	root = filepath.Clean(root)
	if gitDir != filepath.Join(root, ".git") {
		return nil, invalid("unsupported_repository_layout", "Repositories with external Git metadata directories are not supported.")
	}
	project := &Project{Root: root, GitDir: commonDir}
	if projectError := project.validateLayout(); projectError != nil {
		return nil, projectError
	}
	return project, nil
}

func (p *Project) validateLayout() *Error {
	if sparse, err := gitOutput(p.Root, "config", "--bool", "core.sparseCheckout"); err == nil && sparse == "true" {
		return invalid("unsupported_sparse_checkout", "Sparse checkouts are not supported.")
	}
	index, err := gitOutput(p.Root, "ls-files", "--stage")
	if err != nil {
		return internal("inspect Git index", err)
	}
	for _, line := range strings.Split(index, "\n") {
		if strings.HasPrefix(line, "160000 ") {
			return invalid("unsupported_submodule", "Git submodules are not supported.")
		}
	}

	parent := filepath.Dir(p.Root)
	if outerRoot, err := gitOutput(parent, "rev-parse", "--show-toplevel"); err == nil && filepath.Clean(outerRoot) != p.Root {
		return invalid("unsupported_nested_repository", "A Git repository nested inside another repository is not supported.")
	}

	errNestedRepository := errors.New("nested Git repository")
	err = filepath.WalkDir(p.Root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == p.GitDir {
			return filepath.SkipDir
		}
		if path != p.Root && entryAliasesGitMetadata(path, entry) {
			candidateRoot := filepath.Dir(path)
			nestedRoot, nestedErr := gitOutput(candidateRoot, "rev-parse", "--show-toplevel")
			if nestedErr == nil && filepath.Clean(nestedRoot) == candidateRoot {
				return errNestedRepository
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if errors.Is(err, errNestedRepository) {
		return invalid("unsupported_nested_repository", "Nested Git repositories are not supported.")
	}
	if err != nil {
		return internal("inspect repository layout", err)
	}
	return nil
}

func entryAliasesGitMetadata(entryPath string, entry os.DirEntry) bool {
	if entry.Name() == ".git" {
		return true
	}
	entryInfo, err := entry.Info()
	if err != nil {
		return false
	}
	metadataInfo, err := os.Lstat(filepath.Join(filepath.Dir(entryPath), ".git"))
	return err == nil && os.SameFile(entryInfo, metadataInfo)
}

func gitOutput(dir string, args ...string) (string, error) {
	output, err := gitBytes(dir, args...)
	value := strings.TrimSuffix(string(output), "\n")
	return strings.TrimSuffix(value, "\r"), err
}

func gitBytes(dir string, args ...string) ([]byte, error) {
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	return command.Output()
}
