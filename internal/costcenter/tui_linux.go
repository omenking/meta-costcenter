package costcenter

import "syscall"

// The ioctl requests that read and write terminal attributes differ between Linux and macOS.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)
