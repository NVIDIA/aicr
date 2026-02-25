package binding

// Binding in the interface representing a let expression binding.
// You can get the value of the binding by calling the `Value` method.
type Binding interface {
	// Get returns the value bound for a given name.
	Value() (any, error)
}

type binding struct {
	value any
}

func (b *binding) Value() (any, error) {
	return b.value, nil
}

func NewBinding(value any) *binding {
	return &binding{
		value: value,
	}
}

type delegate func() (any, error)

func (b delegate) Value() (any, error) {
	return b()
}

func NewDelegate(delegate func() (any, error)) delegate {
	return delegate
}
