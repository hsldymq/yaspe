package yaspe_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestJobTypeStateCompilation 验证合法构建可编译, 错误构建阶段和不匹配的节点类型被编译器拒绝.
// 临时模块只 import 库, 不递归运行本测试.
func TestJobTypeStateCompilation(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, body, diagnostic string
	}{
		{
			"valid",
			`_, _ = stream.Map(func(v int) string {
    return "ok"
}).TransformFunc(func() (yaspe.Operator[string, bool], error) {
    return operator.NewMap(func(string) bool {
        return true
    }), nil
}).SinkToFunc(func() (yaspe.Sink[bool], error) {
    return nil, nil
}).Build()`,
			"",
		},
		{"draft cannot build", `yaspe.NewJobDraft("x").Build()`, "Build undefined"},
		{"stream cannot build", `stream.Build()`, "Build undefined"},
		{
			"sink ends transforms",
			`stream.SinkToFunc(func() (yaspe.Sink[int], error) {
    return nil, nil
}).Map(func(v int) int {
    return v
})`,
			"Map undefined",
		},
		{
			"wrong map input",
			`stream.Map(func(v string) string {
    return v
})`,
			"does not match",
		},
		{
			"wrong operator input",
			`stream.TransformFunc(func() (yaspe.Operator[string, int], error) {
    return nil, nil
})`,
			"does not match",
		},
		{
			"wrong sink input",
			`stream.SinkToFunc(func() (yaspe.Sink[string], error) {
    return nil, nil
})`,
			"cannot use",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mod := fmt.Sprintf("module contractcheck\n\ngo 1.27\n\nrequire github.com/hsldymq/yaspe v0.0.0\nreplace github.com/hsldymq/yaspe => %q\n", filepath.ToSlash(root))
			source := `package contractcheck
import (
    "github.com/hsldymq/yaspe"
    "github.com/hsldymq/yaspe/operator"
)
var _ = operator.NewMap(func(v int) int {
    return v
})
func check() {
    stream := yaspe.NewJobDraft("x").FromFunc(func() (yaspe.Source[int], error) {
        return nil, nil
    })
    _ = stream
    ` + tc.body + "\n}\n"
			for name, content := range map[string]string{"go.mod": mod, "check.go": source} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "-run=^$", ".")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local")
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("compiler timed out: %s", output)
			}
			if tc.diagnostic == "" {
				if err != nil {
					t.Fatalf("valid API rejected: %v\n%s", err, output)
				}
			} else if err == nil || !strings.Contains(string(output), tc.diagnostic) {
				t.Fatalf("want compiler rejection %q; err=%v\n%s", tc.diagnostic, err, output)
			}
		})
	}
}
