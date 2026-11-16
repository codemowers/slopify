//go:build mips || mipsle || mips64 || mips64le

package linuxioctl

// MIPS uses three direction bits above a 13-bit size. Its NONE/WRITE/READ
// constants are 1/4/2, rather than the generic 0/1/2.
func Request(dir, kind, nr, size uintptr) uintptr {
	direction := [...]uintptr{1, 4, 2, 6}[dir]
	return direction<<29 | size<<16 | kind<<8 | nr
}
