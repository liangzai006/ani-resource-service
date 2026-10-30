package sandbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"k8s.io/client-go/tools/remotecommand"
)

// The expected UID is an argument, never shell source. The container's
// immutable downward-API environment must be checked separately before use.
// This guards the selected container but does not serialize lifecycle actions.
const podUIDGuard = `test "${ANI_SANDBOX_POD_UID:-}" = "$1" || exit 125; shift; exec "$@"`

// Code and stdin travel over the exec stream, not the Kubernetes request URL.
// The ASCII byte length and newline delimit the UTF-8 source from user stdin.
const pythonCodeBootstrap = `import sys; b=sys.stdin.buffer; n=int(b.readline()); c=b.read(n); exec(compile(c,'<sandbox>','exec'),{'__name__':'__main__'})`
const javascriptCodeBootstrap = `(function(){const fs=require('fs'),vm=require('vm');let h='',b=Buffer.alloc(1);for(;;){if(fs.readSync(0,b,0,1,null)!==1)throw Error('missing source');if(b[0]===10)break;h+=String.fromCharCode(b[0]);if(h.length>8)throw Error('invalid source');}let n=Number(h),c=Buffer.alloc(n),i=0;while(i<n){let k=fs.readSync(0,c,i,n-i,null);if(!k)throw Error('short source');i+=k;}vm.runInThisContext(c.toString('utf8'),{filename:'sandbox'});})()`

func guardedCodeRunCommand(podUID string, spec biz.CodeRunSpec) ([]string, error) {
	if podUID == "" || len(podUID) > 128 || strings.ContainsRune(podUID, 0) || spec.Code == "" || strings.ContainsRune(spec.Code, 0) || len(spec.Code) > biz.CodeRunMaxCodeBytes {
		return nil, biz.ErrInvalid
	}
	var program, flag, bootstrap string
	switch spec.Language {
	case "python":
		program, flag, bootstrap = "python3", "-c", pythonCodeBootstrap
	case "javascript":
		program, flag, bootstrap = "node", "-e", javascriptCodeBootstrap
	default:
		return nil, biz.ErrInvalid
	}
	return []string{"/bin/sh", "-c", podUIDGuard, "sandbox-uid-guard", podUID, program, flag, bootstrap}, nil
}

// boundedOutput consumes all output even after the cap is reached, so an
// overproducing process cannot block on a full stream or grow service memory.
type boundedOutput struct {
	mu        sync.Mutex
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	remaining := w.limit - w.data.Len()
	if remaining < 0 {
		remaining = 0
	}
	if len(p) > remaining {
		w.truncated = true
		p = p[:remaining]
	}
	_, _ = w.data.Write(p)
	return n, nil
}

func (w *boundedOutput) result() ([]byte, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data.Bytes()...), w.truncated
}

// codeRunStreamResult reports unknown when the stream does not prove that the
// remote process exited. Closing a Kubernetes exec stream is not a kill receipt.
type codeRunStreamResult struct {
	Stdout, Stderr []byte
	Truncated      bool
	ExitCode       *int
	Unknown        bool
}

// streamCodeRun is the stream-handling part of the adapter, not an admission
// or target check. It must only be called after a fenced dispatch is available.
func streamCodeRun(ctx context.Context, executor remotecommand.Executor, spec biz.CodeRunSpec) (codeRunStreamResult, error) {
	if executor == nil || spec.Timeout <= 0 {
		return codeRunStreamResult{}, biz.ErrInvalid
	}
	runCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	stdout := &boundedOutput{limit: biz.CodeRunOutputBytes}
	stderr := &boundedOutput{limit: biz.CodeRunOutputBytes}
	err := executor.StreamWithContext(runCtx, remotecommand.StreamOptions{
		Stdin: io.MultiReader(strings.NewReader(strconv.Itoa(len(spec.Code))+"\n"), strings.NewReader(spec.Code), strings.NewReader(spec.Stdin)), Stdout: stdout, Stderr: stderr,
	})
	out, truncatedOut := stdout.result()
	errout, truncatedErr := stderr.result()
	result := codeRunStreamResult{Stdout: out, Stderr: errout, Truncated: truncatedOut || truncatedErr}
	if err == nil {
		zero := 0
		result.ExitCode = &zero
		return result, nil
	}
	var exit interface {
		Exited() bool
		ExitStatus() int
	}
	if errors.As(err, &exit) && exit.Exited() {
		code := exit.ExitStatus()
		result.ExitCode = &code
		return result, nil
	}
	result.Unknown = true
	return result, err
}
