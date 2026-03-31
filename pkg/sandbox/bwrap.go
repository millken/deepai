package sandbox

import (
	"errors"
	"os"
	"os/exec"
)

const bwrapPath = "/usr/bin/bwrap"

func hasBubblewrap() bool {
	info, err := os.Stat(bwrapPath)
	return err == nil && !info.IsDir()
}

func bubblewrapArgs(dir string, command string) ([]string, error) {
	if !hasBubblewrap() {
		return nil, errors.New("bubblewrap is not available")
	}

	args := []string{
		bwrapPath,
		"--die-with-parent",
		"--bind", dir, "/workspace",
		"--chdir", "/workspace",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}

	for _, path := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc"} {
		if _, err := os.Stat(path); err == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}

	args = append(args, "/bin/sh", "-lc", command)
	return args, nil
}

func probeBubblewrap(dir string) error {
	args, err := bubblewrapArgs(dir, "true")
	if err != nil {
		return err
	}
	cmd := exec.Command(bwrapPath, args[1:]...)
	return cmd.Run()
}
