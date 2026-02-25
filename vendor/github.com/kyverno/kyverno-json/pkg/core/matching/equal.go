package matching

import (
	"fmt"
	"reflect"

	reflectutils "github.com/kyverno/kyverno-json/pkg/utils/reflect"
)

func Equal(expected, actual any) (bool, error) {
	if actual == nil && expected == nil {
		return true, nil
	} else if actual == nil && expected != nil {
		return false, nil
	} else if actual != nil && expected == nil {
		return false, nil
	}
	if actual == expected {
		return true, nil
	}
	// if they are the same type we can use reflect.DeepEqual
	if reflect.TypeOf(expected) == reflect.TypeOf(actual) {
		return reflect.DeepEqual(expected, actual), nil
	}
	e := reflect.ValueOf(expected)
	a := reflect.ValueOf(actual)
	if !a.IsValid() && !e.IsValid() {
		return true, nil
	}
	if a.CanComplex() && e.CanComplex() {
		return a.Complex() == e.Complex(), nil
	}
	if a.CanFloat() && e.CanFloat() {
		return a.Float() == e.Float(), nil
	}
	if a.CanInt() && e.CanInt() {
		return a.Int() == e.Int(), nil
	}
	if a.CanUint() && e.CanUint() {
		return a.Uint() == e.Uint(), nil
	}
	if a, ok := ToNumber(a); ok {
		if e, ok := ToNumber(e); ok {
			return a == e, nil
		}
	}
	return false, fmt.Errorf("types are not comparable, %s - %s", reflectutils.GetKind(expected), reflectutils.GetKind(actual))
}
