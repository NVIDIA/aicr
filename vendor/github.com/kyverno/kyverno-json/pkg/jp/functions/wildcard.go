package functions

import (
	"errors"

	"github.com/kyverno/pkg/ext/wildcard"
)

func jpfWildcard(arguments []any) (any, error) {
	if pattern, ok := arguments[0].(string); !ok {
		return nil, errors.New("invalid type, first argument must be a string")
	} else if name, ok := arguments[1].(string); !ok {
		return nil, errors.New("invalid type, second argument must be a string")
	} else {
		return wildcard.Match(pattern, name), nil
	}
}
