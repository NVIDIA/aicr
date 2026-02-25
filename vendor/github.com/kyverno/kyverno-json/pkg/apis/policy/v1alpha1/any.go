package v1alpha1

import (
	"github.com/kyverno/kyverno-json/pkg/core/compilers"
	"github.com/kyverno/kyverno-json/pkg/core/projection"
	"github.com/kyverno/kyverno-json/pkg/utils/copy"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// Any can be any type.
// +k8s:deepcopy-gen=false
// +kubebuilder:validation:XPreserveUnknownFields
// +kubebuilder:validation:Type:=""
type Any struct {
	_value any
}

func NewAny(value any) Any {
	return Any{
		_value: value,
	}
}

func (t *Any) IsNil() bool {
	return t._value == nil
}

func (t *Any) Compile(path *field.Path, compilers compilers.Compilers) (projection.ScalarHandler, *field.Error) {
	return projection.ParseScalar(path, t._value, compilers)
}

func (a *Any) MarshalJSON() ([]byte, error) {
	return json.Marshal(a._value)
}

func (a *Any) UnmarshalJSON(data []byte) error {
	var v any
	err := json.Unmarshal(data, &v)
	if err != nil {
		return err
	}
	a._value = v
	return nil
}

func (in *Any) DeepCopyInto(out *Any) {
	out._value = copy.DeepCopy(in._value)
}

func (in *Any) DeepCopy() *Any {
	if in == nil {
		return nil
	}
	out := new(Any)
	in.DeepCopyInto(out)
	return out
}
