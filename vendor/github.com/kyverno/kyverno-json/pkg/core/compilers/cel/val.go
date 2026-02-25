package cel

import (
	"reflect"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
)

type Val[T comparable] struct {
	inner   T
	celType ref.Type
}

func NewVal[T comparable](value T, celType ref.Type) Val[T] {
	return Val[T]{
		inner:   value,
		celType: celType,
	}
}

func (w Val[T]) Unwrap() T {
	return w.inner
}

func (w Val[T]) Value() interface{} {
	return w.Unwrap()
}

func (w Val[T]) ConvertToNative(typeDesc reflect.Type) (interface{}, error) {
	panic("not required")
}

func (w Val[T]) ConvertToType(typeVal ref.Type) ref.Val {
	panic("not required")
}

func (w Val[T]) Equal(other ref.Val) ref.Val {
	o, ok := other.Value().(Val[T])
	if !ok {
		return types.ValOrErr(other, "no such overload")
	}
	return types.Bool(o == w)
}

func (w Val[T]) Type() ref.Type {
	return w.celType
}
