//go:build windows

package plugin

import (
	"fmt"
	"syscall"
)

func dlopen(name string) (uintptr, error) {
	handle, err := syscall.LoadLibrary(name)
	if err != nil {
		return 0, fmt.Errorf("LoadLibrary: %w", err)
	}
	return uintptr(handle), nil
}

func dlsym(lib uintptr, name string) (uintptr, error) {
	proc, err := syscall.GetProcAddress(syscall.Handle(lib), name)
	if err != nil {
		return 0, fmt.Errorf("GetProcAddress %s: %w", name, err)
	}
	return uintptr(proc), nil
}

func dlclose(lib uintptr) {
	syscall.FreeLibrary(syscall.Handle(lib))
}
