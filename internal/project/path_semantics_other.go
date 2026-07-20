//go:build !linux

package project

func directoryPathSemantics(_ string, fallback pathSemantics) (pathSemantics, error) {
	return fallback, nil
}
