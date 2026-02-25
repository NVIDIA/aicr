package v1alpha1

import (
	"github.com/kyverno/kyverno-json/pkg/core/message"
	"k8s.io/apimachinery/pkg/util/json"
)

type _message = message.Message

// Message stores a message template.
// +k8s:deepcopy-gen=false
// +kubebuilder:validation:Type:=string
type Message struct {
	_message
}

func (a *Message) MarshalJSON() ([]byte, error) {
	return json.Marshal(a.Original())
}

func (a *Message) UnmarshalJSON(data []byte) error {
	var v string
	err := json.Unmarshal(data, &v)
	if err != nil {
		return err
	}
	a._message = message.Parse(v)
	return nil
}

func (in *Message) DeepCopyInto(out *Message) {
	out._message = in._message
}

func (in *Message) DeepCopy() *Message {
	if in == nil {
		return nil
	}
	out := new(Message)
	in.DeepCopyInto(out)
	return out
}
