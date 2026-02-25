package functions

import (
	"errors"
)

func jpfConcat(arguments []any) (any, error) {
	if left, ok := arguments[0].(string); !ok {
		return nil, errors.New("invalid type, first argument must be a string")
	} else if right, ok := arguments[1].(string); !ok {
		return nil, errors.New("invalid type, second argument must be a string")
	} else {
		return left + right, nil
	}
}
