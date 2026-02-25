package binding

import (
	"errors"
)

func Resolve(name string, bindings Bindings) (any, error) {
	if bindings == nil {
		return nil, errors.New("bindings must not be nil")
	}
	binding, err := bindings.Get(name)
	if err != nil {
		return nil, err
	}
	return binding.Value()
}
