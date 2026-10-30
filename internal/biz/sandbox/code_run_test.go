package sandbox

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeCodeRun(t *testing.T) {
	spec, err := NormalizeCodeRun(CodeRunInput{Language: " Python ", Code: "print(1)", Stdin: "input"})
	if err != nil || spec.Language != "python" || spec.Timeout != 60*time.Second || spec.Stdin != "input" {
		t.Fatalf("default: %+v %v", spec, err)
	}
	for _, input := range []CodeRunInput{
		{Language: "shell", Code: "true"},
		{Language: "python", Code: " "},
		{Language: "python", Code: "x\x00y"},
		{Language: "python", Code: string([]byte{0xff})},
		{Language: "python", Code: strings.Repeat("x", CodeRunMaxCodeBytes+1)},
		{Language: "python", Code: "x", Stdin: strings.Repeat("x", CodeRunMaxStdinBytes+1)},
		{Language: "python", Code: "x", TimeoutSeconds: 301},
	} {
		if _, err := NormalizeCodeRun(input); !errors.Is(err, ErrInvalid) {
			t.Fatalf("input should fail: language=%q code length=%d timeout=%d: %v", input.Language, len(input.Code), input.TimeoutSeconds, err)
		}
	}
	spec, err = NormalizeCodeRun(CodeRunInput{Language: "javascript", Code: "console.log(1)", TimeoutSeconds: 300})
	if err != nil || spec.Timeout != 300*time.Second {
		t.Fatalf("maximum timeout: %+v %v", spec, err)
	}
}
