//go:build linux

package project

import (
	"errors"

	"golang.org/x/sys/unix"
)

const linuxCasefoldFlag = 0x40000000

func directoryPathSemantics(directory string, fallback pathSemantics) (pathSemantics, error) {
	file, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return pathSemantics{}, err
	}
	defer unix.Close(file)
	flags, err := unix.IoctlGetInt(file, unix.FS_IOC_GETFLAGS)
	if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		return fallback, nil
	}
	if err != nil {
		return pathSemantics{}, err
	}
	casefold := flags&linuxCasefoldFlag != 0
	return pathSemantics{caseFold: casefold, unicodeNormalization: casefold, probeDevice: fallback.probeDevice}, nil
}
