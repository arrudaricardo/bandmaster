package main

import (
	"os"
	"path/filepath"

	"github.com/bandmaster-dev/bandmaster/internal/cli"
	"github.com/ncruces/go-sqlite3"
	"github.com/tetratelabs/wazero"
)

func main() {
	configureSQLiteCompilationCache()
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}

func configureSQLiteCompilationCache() {
	root, err := os.UserCacheDir()
	if err != nil {
		return
	}
	cacheDir := filepath.Join(root, "bandmaster", "wazero")
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return
	}
	cache, err := wazero.NewCompilationCacheWithDir(cacheDir)
	if err != nil {
		return
	}
	sqlite3.RuntimeConfig = wazero.NewRuntimeConfig().WithCompilationCache(cache)
}
