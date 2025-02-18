// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

// MACHINE GENERATED BY 'go generate' COMMAND; DO NOT EDIT

package fingerprint

import (
	"syscall"
	"unsafe"
)

var _ unsafe.Pointer

var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procGetDiskFreeSpaceExW = modkernel32.NewProc("GetDiskFreeSpaceExW")
)

func getDiskFreeSpaceEx(dirName *uint16, availableFreeBytes *uint64, totalBytes *uint64, totalFreeBytes *uint64) (err error) {
	r1, _, e1 := syscall.Syscall6(procGetDiskFreeSpaceExW.Addr(), 4, uintptr(unsafe.Pointer(dirName)), uintptr(unsafe.Pointer(availableFreeBytes)), uintptr(unsafe.Pointer(totalBytes)), uintptr(unsafe.Pointer(totalFreeBytes)), 0, 0)
	if r1 == 0 {
		if e1 != 0 {
			err = error(e1)
		} else {
			err = syscall.EINVAL
		}
	}
	return
}
