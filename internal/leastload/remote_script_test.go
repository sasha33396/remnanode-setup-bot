package leastload

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRemoteScriptsParseAndRollbackDecisionIsExclusive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("MSYS subprocesses under Go are unreliable; run exported shell fixtures directly on Windows")
	}
	bash, err := exec.LookPath("bash")
	if runtime.GOOS == "windows" {
		bash = "C:/Program Files/Git/usr/bin/bash.exe"
		_, err = os.Stat(bash)
	}
	if err != nil {
		t.Skip("bash is unavailable")
	}
	id := "12345678-1234-4234-8234-123456789abc"
	s := &fakeSession{content: source}
	p := Plan{OldIP: "8.8.8.8", NewIP: "8.8.4.4", Mode: "replace", File: "/etc/netplan/provider.yaml", Section: "eth0", FileHash: digest([]byte(source))}
	if err := Start(context.Background(), s, id, p); err != nil {
		t.Fatal(err)
	}
	var rollback string
	for command, script := range s.inputs {
		if !strings.Contains(command, ".sh'") || script == "" {
			continue
		}
		checkCtx, stopCheck := context.WithTimeout(context.Background(), 30*time.Second)
		commandCheck := exec.CommandContext(checkCtx, bash, "--noprofile", "--norc", "-n")
		commandCheck.WaitDelay = time.Second
		commandCheck.Stdin = strings.NewReader(script)
		output, err := commandCheck.CombinedOutput()
		stopCheck()
		if err != nil {
			t.Fatalf("shell syntax: %s %v (context: %v)", output, err, checkCtx.Err())
		}
		if strings.Contains(command, "rollback.sh'") {
			rollback = script
		}
	}
	if rollback == "" {
		t.Fatal("rollback script missing")
	}
	for _, confirmed := range []bool{false, true} {
		dir := filepath.ToSlash(t.TempDir())
		target := dir + "/netplan.yaml"
		for name, content := range map[string]string{"original": "original-config", "netplan.yaml": "new-config", "changed": ""} {
			if err := os.WriteFile(dir+"/"+name, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if confirmed {
			if err := os.WriteFile(dir+"/decision", []byte("confirm"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		script := strings.ReplaceAll(rollback, "/var/lib/remnanode-ip/"+id, dir)
		script = strings.ReplaceAll(script, p.File, target)
		// Only the network command is stubbed; cp, atomic ln and decision logic run.
		script = "netplan() { return 0; }\ntimeout() { shift; \"$@\"; }\n" + script
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		commandRun := exec.CommandContext(ctx, bash, "--noprofile", "--norc", "-s")
		commandRun.WaitDelay = time.Second
		commandRun.Env = append(os.Environ(), "PATH="+filepath.Dir(bash)+string(os.PathListSeparator)+os.Getenv("PATH"))
		commandRun.Stdin = strings.NewReader(script)
		output, err := commandRun.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("rollback script: %s %v", output, err)
		}
		content, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		expected := "original-config"
		if confirmed {
			expected = "new-config"
		}
		if string(content) != expected {
			t.Fatalf("confirmed=%v config=%s output=%s", confirmed, content, output)
		}
		if !confirmed {
			status, err := os.ReadFile(dir + "/status")
			if err != nil || strings.TrimSpace(string(status)) != "rolled_back" {
				t.Fatalf("status=%s %v", status, err)
			}
		}
	}
}

// Export only when explicitly requested by the test runner. All executable
// fixtures use a private test directory and a stub instead of real netplan.
func TestExportRemoteShellFixtures(t *testing.T) {
	base := os.Getenv("LEASTLOAD_SHELL_TEST_DIR")
	if base == "" {
		t.Skip("fixture export not requested")
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	base = filepath.ToSlash(base)
	id := "12345678-1234-4234-8234-123456789abc"
	session := &fakeSession{content: source}
	p := Plan{OldIP: "8.8.8.8", NewIP: "8.8.4.4", Mode: "replace", File: "/etc/netplan/provider.yaml", Section: "eth0", FileHash: digest([]byte(source))}
	if err := Start(context.Background(), session, id, p); err != nil {
		t.Fatal(err)
	}
	var rollback string
	master := "set -eu\nexport PATH=/usr/bin:/bin:$PATH\n"
	for cmd, script := range session.inputs {
		if script == "" || !strings.Contains(cmd, ".sh'") {
			continue
		}
		name := "worker-check.sh"
		if strings.Contains(cmd, "rollback.sh'") {
			name = "rollback-check.sh"
			rollback = script
		}
		if err := os.WriteFile(base+"/"+name, []byte(script), 0600); err != nil {
			t.Fatal(err)
		}
		master += "bash --noprofile --norc -n " + quote(base+"/"+name) + "\n"
	}
	for _, confirmed := range []bool{false, true} {
		dir, err := os.MkdirTemp(base, "rollback-")
		if err != nil {
			t.Fatal(err)
		}
		dir = filepath.ToSlash(dir)
		for name, content := range map[string]string{"original": "original-config", "netplan.yaml": "new-config", "changed": ""} {
			if err := os.WriteFile(dir+"/"+name, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		expected := "original-config"
		if confirmed {
			expected = "new-config"
			if err := os.WriteFile(dir+"/decision", []byte("confirm"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		script := strings.ReplaceAll(rollback, "/var/lib/remnanode-ip/"+id, dir)
		script = strings.ReplaceAll(script, p.File, dir+"/netplan.yaml")
		script = "netplan() { return 0; }\ntimeout() { shift; \"$@\"; }\n" + script + "\n[ \"$(cat " + quote(dir+"/netplan.yaml") + ")\" = " + quote(expected) + " ] || exit 1\nprintf 'rollback decision fixture passed\\n'\n"
		if err := os.WriteFile(dir+"/test.sh", []byte(script), 0600); err != nil {
			t.Fatal(err)
		}
		master += "bash --noprofile --norc " + quote(dir+"/test.sh") + "\n"
	}
	if err := os.WriteFile(base+"/run.sh", []byte(master), 0600); err != nil {
		t.Fatal(err)
	}
}
