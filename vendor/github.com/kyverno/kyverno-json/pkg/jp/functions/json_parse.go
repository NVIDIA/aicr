package functions

import (
	"errors"

	"k8s.io/apimachinery/pkg/util/json"
)

func jpfJsonParse(arguments []any) (any, error) {
	if data, ok := arguments[0].(string); !ok {
		return nil, errors.New("invalid type, first argument must be a string")
	} else {
		var result any
		err := json.Unmarshal([]byte(data), &result)
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}
