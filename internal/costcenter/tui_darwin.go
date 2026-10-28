package costcenter

import "syscall"

// The ioctl requests that read and write terminal attributes differ between Linux and macOS.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
