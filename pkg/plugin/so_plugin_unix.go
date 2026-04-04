//go:build darwin || linux

package plugin

import (
	"fmt"

	"github.com/ebitengine/purego"
)

func dlopen(name string) (uintptr, error) {
	lib, err := purego.Dlopen(name, purego.RTLD_LAZY|purego.RTLD_GLOBAL)
	if err != nil {
		return 0, fmt.Errorf("dlopen: %w", err)
	}
	return lib, nil
}

func dlsym(lib uintptr, name string) (uintptr, error) {
	ptr, err := purego.Dlsym(lib, name)
	if err != nil {
		return 0, fmt.Errorf("dlsym %s: %w", name, err)
	}
	return ptr, nil
}

func dlclose(lib uintptr) {
	purego.Dlclose(lib)
}
