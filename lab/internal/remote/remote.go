package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pfap/lab/internal/model"
)

type Runner struct{}

func (Runner) ScanHostKey(ctx context.Context, s model.Server) (string, error) {
	cmd := exec.CommandContext(ctx, "ssh-keyscan", "-p", strconv.Itoa(s.Port), "-T", "10", s.Host)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("ssh-keyscan: %w: %s", err, bytes.TrimSpace(b))
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return "", errors.New("ssh-keyscan returned no host keys")
	}
	return string(b), nil
}

func sshArgs(s model.Server) []string {
	args := []string{"-p", strconv.Itoa(s.Port), "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes"}
	if s.IdentityFile != "" {
		args = append(args, "-i", s.IdentityFile)
	}
	if s.KnownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+s.KnownHostsFile)
	}
	return append(args, s.User+"@"+s.Host)
}

func (Runner) Run(ctx context.Context, s model.Server, script string) (string, error) {
	if s.Host == "local" || s.Host == "localhost-local" {
		cmd := exec.CommandContext(ctx, "bash", "-s")
		cmd.Stdin = strings.NewReader(script)
		b, err := cmd.CombinedOutput()
		return string(b), err
	}
	args := append(sshArgs(s), "bash -s")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = strings.NewReader(script)
	b, err := cmd.CombinedOutput()
	if err != nil {
		return string(b), fmt.Errorf("ssh: %w: %s", err, bytes.TrimSpace(b))
	}
	return string(b), nil
}

func (Runner) Copy(ctx context.Context, s model.Server, source, destination string) error {
	if s.Host == "local" || s.Host == "localhost-local" {
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return err
		}
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(destination)
		if err != nil {
			return err
		}
		if _, err = io.Copy(out, in); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	}
	args := []string{"-P", strconv.Itoa(s.Port), "-q", "-o", "StrictHostKeyChecking=yes"}
	if s.IdentityFile != "" {
		args = append(args, "-i", s.IdentityFile)
	}
	if s.KnownHostsFile != "" {
		args = append(args, "-o", "UserKnownHostsFile="+s.KnownHostsFile)
	}
	args = append(args, source, s.User+"@"+s.Host+":"+destination)
	if b, err := exec.CommandContext(ctx, "scp", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("scp: %w: %s", err, bytes.TrimSpace(b))
	}
	return nil
}
