//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	landlockCreateRulesetVersion = unix.LANDLOCK_CREATE_RULESET_VERSION
)

var landlockAccessFS = uint64(
	unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
		unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
		unix.LANDLOCK_ACCESS_FS_MAKE_REG |
		unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
		unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_SYM |
		unix.LANDLOCK_ACCESS_FS_REFER |
		unix.LANDLOCK_ACCESS_FS_TRUNCATE |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV,
)

var landlockRuntimeAccessFS = uint64(
	unix.LANDLOCK_ACCESS_FS_EXECUTE |
		unix.LANDLOCK_ACCESS_FS_READ_FILE |
		unix.LANDLOCK_ACCESS_FS_READ_DIR |
		unix.LANDLOCK_ACCESS_FS_IOCTL_DEV,
)

// CheckLandlockAvailable reports whether the current kernel exposes Landlock.
func CheckLandlockAvailable() bool {
	abi, err := landlockCreateRuleset(nil, 0, landlockCreateRulesetVersion)
	if err != nil {
		return false
	}
	return abi >= 1
}

func probeLandlock(sessionDir string) error {
	rulesetFD, err := createRuleset()
	if err != nil {
		return err
	}
	defer unix.Close(rulesetFD)

	return addLandlockRules(rulesetFD, sessionDir)
}

func applyLandlock(sessionDir string) error {
	rulesetFD, err := createRuleset()
	if err != nil {
		return err
	}
	defer unix.Close(rulesetFD)

	if err := addLandlockRules(rulesetFD, sessionDir); err != nil {
		return err
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if err := landlockRestrictSelf(rulesetFD, 0); err != nil {
		return fmt.Errorf("landlock restrict self: %w", err)
	}
	return nil
}

func addLandlockRules(rulesetFD int, sessionDir string) error {
	if err := addLandlockRule(rulesetFD, sessionDir, landlockAccessFS); err != nil {
		return fmt.Errorf("landlock add session rule: %w", err)
	}

	for _, path := range []string{"/bin", "/usr", "/lib", "/lib64", "/etc"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := addLandlockRule(rulesetFD, path, landlockRuntimeAccessFS); err != nil {
			return fmt.Errorf("landlock add runtime rule for %s: %w", path, err)
		}
	}

	return nil
}

func addLandlockRule(rulesetFD int, path string, access uint64) error {
	dirFD, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open path %s: %w", path, err)
	}
	defer unix.Close(dirFD)

	attr := unix.LandlockPathBeneathAttr{
		Allowed_access: access,
		Parent_fd:      int32(dirFD),
	}
	if err := landlockAddRule(rulesetFD, unix.LANDLOCK_RULE_PATH_BENEATH, &attr, 0); err != nil {
		return err
	}
	return nil
}

func createRuleset() (int, error) {
	attr := unix.LandlockRulesetAttr{
		Access_fs: landlockAccessFS,
	}
	return landlockCreateRuleset(&attr, unsafe.Sizeof(attr), 0)
}

func landlockCreateRuleset(attr *unix.LandlockRulesetAttr, size uintptr, flags uint32) (int, error) {
	var ptr uintptr
	if attr != nil {
		ptr = uintptr(unsafe.Pointer(attr))
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, ptr, size, uintptr(flags))
	if errno != 0 {
		return -1, normalizeLandlockError(errno)
	}
	return int(fd), nil
}

func landlockAddRule(rulesetFD int, ruleType int, attr *unix.LandlockPathBeneathAttr, flags uint32) error {
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(rulesetFD),
		uintptr(ruleType),
		uintptr(unsafe.Pointer(attr)),
		uintptr(flags),
		0,
		0,
	)
	if errno != 0 {
		return normalizeLandlockError(errno)
	}
	return nil
}

func landlockRestrictSelf(rulesetFD int, flags uint32) error {
	_, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(rulesetFD), uintptr(flags), 0)
	if errno != 0 {
		return normalizeLandlockError(errno)
	}
	return nil
}

func normalizeLandlockError(errno unix.Errno) error {
	switch errno {
	case 0:
		return nil
	case unix.ENOSYS, unix.EOPNOTSUPP:
		return fmt.Errorf("landlock unavailable (kernel 5.13+ required): %w", errno)
	default:
		return os.NewSyscallError("landlock", errors.New(errno.Error()))
	}
}
