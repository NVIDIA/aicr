package v1alpha1

import (
	"github.com/kyverno/kyverno-json/pkg/core/assertion"
	"github.com/kyverno/kyverno-json/pkg/core/compilers"
	"github.com/kyverno/kyverno-json/pkg/utils/copy"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

// +k8s:deepcopy-gen=false
// +kubebuilder:validation:XPreserveUnknownFields
// +kubebuilder:validation:Type:=""
// AssertionTree represents an assertion tree.
type AssertionTree struct {
	_tree any
}

func NewAssertionTree(value any) AssertionTree {
	return AssertionTree{
		_tree: value,
	}
}

func (t *AssertionTree) IsNil() bool {
	return t._tree == nil
}

func (t *AssertionTree) Compile(path *field.Path, compilers compilers.Compilers) (assertion.Assertion, *field.Error) {
	return assertion.Parse(path, t._tree, compilers)
}

func (a *AssertionTree) MarshalJSON() ([]byte, error) {
	return json.Marshal(a._tree)
}

func (a *AssertionTree) UnmarshalJSON(data []byte) error {
	var v any
	err := json.Unmarshal(data, &v)
	if err != nil {
		return err
	}
	a._tree = v
	return nil
}

func (in *AssertionTree) DeepCopyInto(out *AssertionTree) {
	out._tree = copy.DeepCopy(in._tree)
}

func (in *AssertionTree) DeepCopy() *AssertionTree {
	if in == nil {
		return nil
	}
	out := new(AssertionTree)
	in.DeepCopyInto(out)
	return out
}
