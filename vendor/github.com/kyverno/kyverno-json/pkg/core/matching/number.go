package matching

import (
	"reflect"
)

func ToNumber(value reflect.Value) (float64, bool) {
	if value.CanFloat() {
		return value.Float(), true
	}
	if value.CanInt() {
		return float64(value.Int()), true
	}
	if value.CanUint() {
		return float64(value.Uint()), true
	}
	return 0, false
}
