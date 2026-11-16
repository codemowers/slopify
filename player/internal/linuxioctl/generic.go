//go:build !mips && !mipsle && !mips64 && !mips64le

// Package linuxioctl encodes Linux ioctl requests without depending on libc.
package linuxioctl

// Request takes generic directions: 0 none, 1 write, 2 read, 3 read/write.
func Request(dir, kind, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | kind<<8 | nr
}
