//go:build unix

package source

import "syscall"

// syscallNoFollow makes opening a planted symlink fail instead of following it.
const syscallNoFollow = syscall.O_NOFOLLOW
