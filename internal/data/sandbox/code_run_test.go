package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"k8s.io/client-go/tools/remotecommand"
)

type fakeCodeRunStream func(context.Context, remotecommand.StreamOptions) error

func (f fakeCodeRunStream) Stream(o remotecommand.StreamOptions) error {
	return f(context.Background(), o)
}
func (f fakeCodeRunStream) StreamWithContext(ctx context.Context, o remotecommand.StreamOptions) error {
	return f(ctx, o)
}

type codeRunExit int

func (e codeRunExit) Error() string   { return "process exited" }
func (e codeRunExit) Exited() bool    { return true }
func (e codeRunExit) ExitStatus() int { return int(e) }

func TestGuardedCodeRunCommandChecksUIDAndKeepsCodeLiteral(t *testing.T) {
	dir := t.TempDir()
	program := filepath.Join(dir, "python3")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s' \"$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	code := "hello; touch " + filepath.Join(dir, "escaped")
	args, err := guardedCodeRunCommand("expected-uid", biz.CodeRunSpec{Language: "python", Code: code})
	if err != nil {
		t.Fatal(err)
	}
	run := func(uid string) ([]byte, error) {
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Env = []string{"PATH=" + dir, "ANI_SANDBOX_POD_UID=" + uid}
		return cmd.CombinedOutput()
	}
	if out, err := run("other-uid"); err == nil || len(out) != 0 {
		t.Fatalf("wrong UID executed: output=%q err=%v", out, err)
	}
	if out, err := run("expected-uid"); err != nil || string(out) != pythonCodeBootstrap {
		t.Fatalf("matching UID: output=%q err=%v", out, err)
	}
	if strings.Contains(strings.Join(args, " "), code) {
		t.Fatal("submitted code appeared in the exec URL arguments")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("code was interpreted as shell: %v", err)
	}
}

func TestCodeRunBootstrapStreamsSourceAndStdin(t *testing.T) {
	for _, tc := range []struct{ language, executable, code string }{
		{"python", "python3", "print(input())"},
		{"javascript", "node", "const fs=require('fs'); console.log(fs.readFileSync(0,'utf8').trim())"},
	} {
		t.Run(tc.language, func(t *testing.T) {
			if _, err := exec.LookPath(tc.executable); err != nil {
				t.Skip("language runtime not installed")
			}
			args, err := guardedCodeRunCommand("test-uid", biz.CodeRunSpec{Language: tc.language, Code: tc.code})
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(args[0], args[1:]...)
			cmd.Env = append(os.Environ(), "ANI_SANDBOX_POD_UID=test-uid")
			cmd.Stdin = strings.NewReader(strconv.Itoa(len(tc.code)) + "\n" + tc.code + "payload\n")
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "payload\n" {
				t.Fatalf("output=%q err=%v", out, err)
			}
		})
	}
}

func TestBoundedOutputConsumesAndCaps(t *testing.T) {
	w := &boundedOutput{limit: 5}
	for _, input := range []string{"abc", "def", strings.Repeat("x", 100000)} {
		n, err := w.Write([]byte(input))
		if n != len(input) || err != nil {
			t.Fatalf("write consumed %d/%d: %v", n, len(input), err)
		}
	}
	got, truncated := w.result()
	if !bytes.Equal(got, []byte("abcde")) || !truncated {
		t.Fatalf("got %q, truncated=%t", got, truncated)
	}
	got[0] = 'z'
	got, _ = w.result()
	if string(got) != "abcde" {
		t.Fatal("result aliases internal buffer")
	}
}

func TestCodeRunStreamBoundedOutputAndUnknownTimeout(t *testing.T) {
	spec := biz.CodeRunSpec{Code: "print(1)", Stdin: "input", Timeout: time.Second}
	stream := fakeCodeRunStream(func(_ context.Context, o remotecommand.StreamOptions) error {
		in, err := io.ReadAll(o.Stdin)
		if err != nil || string(in) != "8\nprint(1)input" {
			t.Fatalf("stdin=%q err=%v", in, err)
		}
		_, _ = io.WriteString(o.Stdout, strings.Repeat("a", biz.CodeRunOutputBytes+1))
		_, _ = io.WriteString(o.Stderr, "diagnostic")
		return codeRunExit(7)
	})
	result, err := streamCodeRun(context.Background(), stream, spec)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 7 || !result.Truncated || len(result.Stdout) != biz.CodeRunOutputBytes || string(result.Stderr) != "diagnostic" || result.Unknown {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	stream = func(ctx context.Context, _ remotecommand.StreamOptions) error {
		<-ctx.Done()
		return ctx.Err()
	}
	spec.Timeout = time.Millisecond
	result, err = streamCodeRun(context.Background(), stream, spec)
	if !errors.Is(err, context.DeadlineExceeded) || !result.Unknown || result.ExitCode != nil {
		t.Fatalf("timeout incorrectly treated as a confirmed exit: %+v %v", result, err)
	}
}
