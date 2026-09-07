package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pfap/lab/internal/model"
)

func rotationFixture(t *testing.T) (model.Experiment, model.Node, model.Server, string) {
	t.Helper()
	exp := model.Experiment{ID: "exp-logs"}
	server := model.Server{ID: "worker-logs", Host: "local", WorkDir: t.TempDir()}
	node := model.Node{ID: "node-logs", ServerID: server.ID, LocalIndex: 1}
	dir := filepath.Join(server.WorkDir, "experiments", exp.ID, server.ID, "node1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return exp, node, server, filepath.Join(dir, "geth.log")
}

func runRotation(t *testing.T, exp model.Experiment, node model.Node, server model.Server, max int64) (string, error) {
	t.Helper()
	script, err := logRotationScript(exp, node, server, max)
	if err != nil {
		return "", err
	}
	return (Orchestrator{}).Remote.Run(context.Background(), server, script)
}

func TestLogRotationPreservesOpenDescriptorAndPrivateState(t *testing.T) {
	exp, node, server, log := rotationFixture(t)
	writer, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	before := "existing geth log exceeding test rotation threshold\n"
	if _, err := writer.WriteString(before); err != nil {
		t.Fatal(err)
	}
	info, err := writer.Stat()
	if err != nil {
		t.Fatal(err)
	}
	protected := map[string]string{"SN": "SN state", "keystore/account": "secret", "geth/chaindata/00001.ldb": "chain data"}
	for name, contents := range protected {
		path := filepath.Join(filepath.Dir(log), name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runRotation(t, exp, node, server, 16)
	if err != nil || !strings.Contains(out, "log_rotation=rotated") {
		t.Fatalf("rotation: %q %v", out, err)
	}
	afterInfo, err := os.Stat(log)
	if err != nil || !os.SameFile(info, afterInfo) || afterInfo.Size() != 0 {
		t.Fatalf("active log inode was replaced or not truncated: %+v %v", afterInfo, err)
	}
	if _, err := writer.WriteString("continued logging\n"); err != nil {
		t.Fatal(err)
	}
	if got := testRead(t, log); got != "continued logging\n" {
		t.Fatalf("original descriptor no longer writes active log: %q", got)
	}
	if got := testRead(t, log+".1"); got != before {
		t.Fatalf("log archive contents: %q", got)
	}
	for name, contents := range protected {
		if got := testRead(t, filepath.Join(filepath.Dir(log), name)); got != contents {
			t.Fatalf("rotation changed %s: %q", name, got)
		}
	}
}

func TestLogRotationRetainsThreeArchivesInOrder(t *testing.T) {
	exp, node, server, log := rotationFixture(t)
	for generation := 1; generation <= 4; generation++ {
		if err := os.WriteFile(log, []byte(fmt.Sprintf("generation-%d\n", generation)), 0600); err != nil {
			t.Fatal(err)
		}
		if out, err := runRotation(t, exp, node, server, 8); err != nil {
			t.Fatalf("rotation %d: %q %v", generation, out, err)
		}
	}
	for archive := 1; archive <= 3; archive++ {
		if got, want := testRead(t, fmt.Sprintf("%s.%d", log, archive)), fmt.Sprintf("generation-%d\n", 5-archive); got != want {
			t.Fatalf("archive %d: got %q want %q", archive, got, want)
		}
	}
	if _, err := os.Stat(log + ".4"); !os.IsNotExist(err) {
		t.Fatalf("unexpected fourth archive: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(log))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".lab-log-rotation.") {
			t.Fatalf("temporary archive left behind: %s", entry.Name())
		}
	}
}

func TestLogRotationSkipsMissingAndSmallLogs(t *testing.T) {
	exp, node, server, log := rotationFixture(t)
	if out, err := runRotation(t, exp, node, server, 16); err != nil || !strings.Contains(out, "log_rotation=missing") {
		t.Fatalf("missing log: %q %v", out, err)
	}
	if err := os.WriteFile(log, []byte("small"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := runRotation(t, exp, node, server, 16); err != nil || !strings.Contains(out, "log_rotation=below-limit") {
		t.Fatalf("small log: %q %v", out, err)
	}
	if got := testRead(t, log); got != "small" {
		t.Fatalf("small log changed: %q", got)
	}
}

func TestLogRotationRejectsUnsafeLinks(t *testing.T) {
	for _, kind := range []string{"log-symlink", "log-hardlink", "archive-symlink", "archive-hardlink", "archive-directory", "directory-symlink", "lock-symlink", "lock-hardlink", "lock-directory"} {
		t.Run(kind, func(t *testing.T) {
			exp, node, server, log := rotationFixture(t)
			original := "original log exceeding threshold\n"
			if err := os.WriteFile(log, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "protected")
			if err := os.WriteFile(outside, []byte("do not change"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "log-symlink":
				if err = os.Remove(log); err == nil {
					err = os.Symlink(outside, log)
				}
			case "log-hardlink":
				err = os.Link(log, outside+"-link")
			case "archive-symlink":
				err = os.Symlink(outside, log+".1")
			case "archive-hardlink":
				err = os.Link(outside, log+".2")
			case "archive-directory":
				err = os.Mkdir(log+".3", 0700)
			case "lock-symlink":
				err = os.Symlink(outside, filepath.Join(filepath.Dir(log), ".lab-log-maintenance.lock"))
			case "lock-hardlink":
				err = os.Link(outside, filepath.Join(filepath.Dir(log), ".lab-log-maintenance.lock"))
			case "lock-directory":
				err = os.Mkdir(filepath.Join(filepath.Dir(log), ".lab-log-maintenance.lock"), 0700)
			case "directory-symlink":
				dir := filepath.Dir(log)
				moved := filepath.Join(t.TempDir(), "logs")
				if err = os.Rename(dir, moved); err == nil {
					err = os.Symlink(moved, dir)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if out, err := runRotation(t, exp, node, server, 8); err == nil {
				t.Fatalf("unsafe %s accepted: %q", kind, out)
			}
			if got := testRead(t, outside); got != "do not change" {
				t.Fatalf("outside file changed: %q", got)
			}
			if kind != "log-symlink" {
				if got := testRead(t, log); got != original {
					t.Fatalf("active log changed on refusal: %q", got)
				}
			}
		})
	}
}

func TestLogRotationDeadlineKillsStalledCopyWithoutLateTruncation(t *testing.T) {
	exp, node, server, log := rotationFixture(t)
	original := "active log must survive a stalled archive copy\n"
	writer, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.WriteString(original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(log+".1", []byte("previous archive\n"), 0600); err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := writer.Stat()
	if err != nil {
		t.Fatal(err)
	}
	realCopy, err := exec.LookPath("cp")
	if err != nil {
		t.Fatal(err)
	}
	shimDir := t.TempDir()
	copyPID, sleepPID := filepath.Join(shimDir, "copy.pid"), filepath.Join(shimDir, "sleep.pid")
	completed := filepath.Join(shimDir, "copy.completed")
	shim := "#!/bin/bash\n" +
		"printf '%s\\n' \"$$\" >" + shell(copyPID) + "\n" +
		"sleep 1 &\n" +
		"printf '%s\\n' \"$!\" >" + shell(sleepPID) + "\n" +
		"wait \"$!\"\n" + shell(realCopy) + " \"$@\"\n" +
		"printf 'completed\\n' >" + shell(completed) + "\n"
	if err := os.WriteFile(filepath.Join(shimDir, "cp"), []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+":"+os.Getenv("PATH"))
	script, err := logRotationScript(exp, node, server, 8)
	if err != nil {
		t.Fatal(err)
	}
	wrapper := "timeout -k 1s " + strconv.Itoa(int(logRotationDeadline/time.Second)) + "s bash -c "
	if strings.Count(script, wrapper) != 1 {
		t.Fatalf("rotation is missing its worker deadline: %q", script)
	}
	// Shorten only this generated test script; production uses its full lease.
	script = strings.Replace(script, wrapper, "timeout -k 0.2s 0.2s bash -c ", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	out, err := (Orchestrator{}).Remote.Run(ctx, server, script)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || (exitErr.ExitCode() != 124 && exitErr.ExitCode() != 137) || strings.Contains(out, "log_rotation=rotated") {
		t.Fatalf("stalled copy did not stop at the worker deadline: %q %v", out, err)
	}
	if elapsed := time.Since(started); elapsed >= 900*time.Millisecond {
		t.Fatalf("worker waited for the stalled copy instead of killing its group: %s", elapsed)
	}
	for _, pidFile := range []string{copyPID, sleepPID} {
		pid, err := strconv.Atoi(strings.TrimSpace(testRead(t, pidFile)))
		if err != nil || pid < 1 {
			t.Fatalf("invalid stalled helper identity: %v", err)
		}
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		state := strings.LastIndex(string(stat), ") ") + 2
		if state < 2 || state >= len(stat) || (stat[state] != 'Z' && stat[state] != 'X') {
			t.Fatalf("stalled helper %d survived the process-group deadline", pid)
		}
	}
	if _, err := writer.WriteString("continued after deadline\n"); err != nil {
		t.Fatal(err)
	}
	// Cross the fake copy's original completion time to detect any late copy,
	// archive rename or truncation after the controller would release its locks.
	time.Sleep(time.Until(started.Add(1200 * time.Millisecond)))
	if got := testRead(t, log); got != original+"continued after deadline\n" {
		t.Fatalf("active log was truncated after the deadline: %q", got)
	}
	afterInfo, err := os.Stat(log)
	if err != nil || !os.SameFile(beforeInfo, afterInfo) {
		t.Fatalf("active log inode changed: %v", err)
	}
	if got := testRead(t, log+".1"); got != "previous archive\n" {
		t.Fatalf("archive completed after the deadline: %q", got)
	}
	for _, path := range []string{log + ".2", completed} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stalled copy completed later: %s %v", path, err)
		}
	}
}

func TestRetainedLogTailIncludesBoundaryAcrossRotation(t *testing.T) {
	_, _, server, log := rotationFixture(t)
	for name, contents := range map[string]string{
		log + ".3": "oldest\n",
		log + ".2": "older\n",
		log + ".1": "generation start\nproof Use Time: 42\n",
		log:        "generation finish\nnewest\n",
	} {
		if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := (Orchestrator{}).Remote.Run(context.Background(), server, retainedLogTail(log, 10))
	if err != nil {
		t.Fatal(err)
	}
	if want := "oldest\nolder\ngeneration start\nproof Use Time: 42\ngeneration finish\nnewest\n"; out != want {
		t.Fatalf("retained timing boundary order: got %q want %q", out, want)
	}
}
