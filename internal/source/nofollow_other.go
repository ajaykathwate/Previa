//go:build !unix

package source

// syscallNoFollow is unavailable here; workspaces are supported on Unix workers.
const syscallNoFollow = 0
