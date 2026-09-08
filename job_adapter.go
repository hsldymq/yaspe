package yaspe

import "reflect"

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

// 类型元信息和异构 Factory 只存在于私有 adapter, 不要求用户提供 TypeHint.
type nodeSignature struct {
	kind   nodeKind
	input  reflect.Type
	output reflect.Type
}

type definitionAdapter interface {
	signature() nodeSignature
	nilFactory() bool
}

type sourceAdapter[T any] struct {
	factory SourceFactory[T]
}

func (a sourceAdapter[T]) signature() nodeSignature {
	return nodeSignature{kind: sourceNode, output: reflect.TypeFor[T]()}
}
func (a sourceAdapter[T]) nilFactory() bool { return isNil(a.factory) }

type operatorAdapter[I, O any] struct{ factory OperatorFactory[I, O] }

func (a operatorAdapter[I, O]) signature() nodeSignature {
	return nodeSignature{kind: operatorNode, input: reflect.TypeFor[I](), output: reflect.TypeFor[O]()}
}
func (a operatorAdapter[I, O]) nilFactory() bool { return isNil(a.factory) }

type sinkAdapter[T any] struct {
	factory SinkFactory[T]
}

func (a sinkAdapter[T]) signature() nodeSignature {
	return nodeSignature{kind: sinkNode, input: reflect.TypeFor[T]()}
}
func (a sinkAdapter[T]) nilFactory() bool { return isNil(a.factory) }

// persistent 上游链允许低成本派生, Build 则分配独立的拓扑快照.
type definitionNode struct {
	upstream  *definitionNode
	signature nodeSignature
	adapter   definitionAdapter
}

func newDefinitionNode(upstream *definitionNode, adapter definitionAdapter) *definitionNode {
	return &definitionNode{upstream: upstream, signature: adapter.signature(), adapter: adapter}
}

type transformation struct {
	id        int
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
