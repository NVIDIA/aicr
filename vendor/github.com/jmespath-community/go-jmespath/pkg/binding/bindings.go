package binding

import (
	"fmt"
)

// Bindings stores let expression bindings by name.
type Bindings interface {
	// Get returns the binding for a given name.
	Get(string) (Binding, error)
	// Register registers a binding associated with a given name, it returns a new binding
	Register(string, Binding) Bindings
}

type bindings struct {
	bindings map[string]Binding
}

func NewBindings() Bindings {
	return bindings{}
}

func (b bindings) Get(name string) (Binding, error) {
	if value, ok := b.bindings[name]; ok {
		return value, nil
	}
	return nil, fmt.Errorf("variable not defined: %s", name)
}

func (b bindings) Register(name string, binding Binding) Bindings {
	values := map[string]Binding{}
	for k, v := range b.bindings {
		values[k] = v
	}
	values[name] = binding
	return bindings{
		bindings: values,
	}
}
