package yaspe

import (
	"fmt"
	"strings"
)

// JobDraft 是尚未绑定 Source 的不可变定义; 零值无效.
type JobDraft struct {
	name  string
	valid bool
}

// JobBuilder 是已有 Source 和 Sink, 可通过 Build 校验并生成 Job 的不可变定义.
// 零值无效.
type JobBuilder struct {
	name  string
	tail  *definitionNode
	valid bool
}

// Job 保存经过校验的不可变线性拓扑, 不持有运行实例或运行资源.
// 每次 Build 复制 yaspe 拥有的数据; 用户 Factory 和函数的可达对象不会被深拷贝.
// Job 可供不同 Runtime 重复并发执行; 零值无效.
type Job struct {
	name  string
	nodes []transformation
}

// Name 返回定义时的 Job 名称.
func (j Job) Name() string {
	return j.name
}

// NewJobDraft 开始一个新定义. 空白名称在 Build 时统一报告错误.
func NewJobDraft(name string) JobDraft {
	return JobDraft{
		name:  name,
		valid: true,
	}
}

// From 保存 Source Factory, 不调用 Create.
func (d JobDraft) From[T any](factory SourceFactory[T]) Stream[T] {
	return Stream[T]{
		name:  d.name,
		valid: d.valid,
		tail: newDefinitionNode(nil, sourceAdapter[T]{
			factory,
		}),
	}
}

// FromFunc 是 From 的函数形式, 工厂必须遵循 SourceFactory 的生命周期与并发契约.
func (d JobDraft) FromFunc[T any](factory func() (Source[T], error)) Stream[T] {
	return d.From[T](sourceFactoryFunc[T](factory))
}

// BuildError 表示定义期校验失败, 不是运行期或 Factory 创建错误.
type BuildError struct {
	node   int
	reason string
}

func (e *BuildError) Error() string {
	if e.node < 0 {
		return "yaspe: invalid job: " + e.reason
	}
	return fmt.Sprintf("yaspe: invalid job node %d: %s", e.node, e.reason)
}

// Build 校验唯一 Source, 线性 Operator Chain, 唯一 Sink, 并复制节点关系.
// 不调用用户函数, Factory 或 Open, 不创建 goroutine 或访问外部系统.
// 对同一 Builder 可重复或并发调用; 错误确定, 成功快照互相独立.
func (b JobBuilder) Build() (Job, error) {
	fail := func(node int, reason string) (Job, error) {
		return Job{}, &BuildError{
			node:   node,
			reason: reason,
		}
	}
	if !b.valid {
		return fail(-1, "zero or invalid builder")
	}
	if strings.TrimSpace(b.name) == "" {
		return fail(-1, "name must not be blank")
	}

	// 遍历不可变上游引用, 不对共享的定义节点写入结构序号.
	var path []*definitionNode
	seen := make(map[*definitionNode]struct{})
	for node := b.tail; node != nil; node = node.upstream {
		if _, exists := seen[node]; exists {
			return fail(-1, "cycle or repeated node")
		}
		seen[node] = struct{}{}
		path = append(path, node)
	}
	if len(path) < 2 {
		return fail(-1, "source and sink are required")
	}

	nodes := make([]transformation, len(path))
	for i := range nodes {
		node := path[len(path)-1-i]
		wantKind := operatorNode
		if i == 0 {
			wantKind = sourceNode
		}
		if i == len(nodes)-1 {
			wantKind = sinkNode
		}
		if node.signature.kind != wantKind {
			return fail(i, "expected "+wantKind.String())
		}
		if node.adapter == nil || isNil(node.adapter) {
			return fail(i, "missing typed adapter")
		}
		if node.signature != node.adapter.signature() {
			return fail(i, "inconsistent adapter type metadata")
		}
		if node.adapter.nilFactory() {
			return fail(i, "nil factory or function")
		}
		if i > 0 && nodes[i-1].signature.output != node.signature.input {
			return fail(i, "incompatible upstream output and input types")
		}
		nodes[i] = transformation{
			id:        i,
			signature: node.signature,
			adapter:   node.adapter,
		}
		if i > 0 {
			nodes[i].upstream = &nodes[i-1]
		}
	}
	return Job{
		name:  b.name,
		nodes: nodes,
	}, nil
}
