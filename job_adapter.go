package yaspe

import "reflect"

// nodeKind 区分 Source, Operator 和 Sink, 用于校验线性拓扑中的节点职责.
type nodeKind uint8

const (
	sourceNode nodeKind = iota + 1
	operatorNode
	sinkNode
)

func (k nodeKind) String() string {
	switch k {
	case sourceNode:
		return "source"
	case operatorNode:
		return "operator"
	case sinkNode:
		return "sink"
	default:
		return "invalid node"
	}
}

// nodeSignature 描述节点职责及输入输出类型, 用于 Build 校验相邻节点是否匹配.
type nodeSignature struct {
	kind nodeKind
	// Source 没有输入类型, 此字段为 nil.
	input reflect.Type
	// Sink 没有输出类型, 此字段为 nil.
	output reflect.Type
}

// definitionAdapter 为异构泛型工厂提供定义期类型信息及 nil 校验, 不在 Build 中创建实例.
type definitionAdapter interface {
	signature() nodeSignature
	nilFactory() bool
}

// sourceAdapter 保存类型安全的 Source 工厂, 供定义校验与运行实例化使用.
type sourceAdapter[T any] struct {
	factory SourceFactory[T]
}

func (a sourceAdapter[T]) signature() nodeSignature {
	return nodeSignature{
		kind:   sourceNode,
		output: reflect.TypeFor[T](),
	}
}
func (a sourceAdapter[T]) nilFactory() bool {
	return isNil(a.factory)
}

// operatorAdapter 保存输入和输出类型确定的 Operator 工厂, 不共享运行实例.
type operatorAdapter[I, O any] struct {
	factory OperatorFactory[I, O]
}

func (a operatorAdapter[I, O]) signature() nodeSignature {
	return nodeSignature{
		kind:   operatorNode,
		input:  reflect.TypeFor[I](),
		output: reflect.TypeFor[O](),
	}
}
func (a operatorAdapter[I, O]) nilFactory() bool {
	return isNil(a.factory)
}

// sinkAdapter 保存 Sink 工厂及其实际输入类型.
type sinkAdapter[T any] struct {
	factory SinkFactory[T]
}

func (a sinkAdapter[T]) signature() nodeSignature {
	return nodeSignature{
		kind:  sinkNode,
		input: reflect.TypeFor[T](),
	}
}
func (a sinkAdapter[T]) nilFactory() bool {
	return isNil(a.factory)
}

// definitionNode 是不可变的构建节点, 派生路径共享上游, Build 时复制为独立快照.
type definitionNode struct {
	upstream  *definitionNode
	signature nodeSignature
	adapter   definitionAdapter
}

func newDefinitionNode(upstream *definitionNode, adapter definitionAdapter) *definitionNode {
	return &definitionNode{
		upstream:  upstream,
		signature: adapter.signature(),
		adapter:   adapter,
	}
}

// transformation 是 Build 产生的 Job 私有节点快照, 节点关系只属于当前 Job.
type transformation struct {
	// 当前 Job 内按线性拓扑顺序分配的序号, 不是跨运行持久标识.
	id int
	// 只指向同一 Job 快照内的前序节点, Source 为 nil.
	upstream  *transformation
	signature nodeSignature
	adapter   definitionAdapter
}

// typed nil Factory 也必须拒绝; 检查不调用 Factory 的任何方法.
func isNil(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}
