package matching

import (
	"fmt"
	"reflect"

	reflectutils "github.com/kyverno/kyverno-json/pkg/utils/reflect"
)

func Match(expected, actual any) (bool, error) {
	if expected != nil {
		switch reflectutils.GetKind(expected) {
		case reflect.Slice:
			if reflectutils.GetKind(actual) != reflect.Slice {
				return false, fmt.Errorf("invalid actual value, must be a slice, found %s", reflectutils.GetKind(actual))
			}
			if reflect.ValueOf(expected).Len() != reflect.ValueOf(actual).Len() {
				return false, nil
			}
			for i := 0; i < reflect.ValueOf(expected).Len(); i++ {
				if inner, err := Match(reflect.ValueOf(expected).Index(i).Interface(), reflect.ValueOf(actual).Index(i).Interface()); err != nil {
					return false, err
				} else if !inner {
					return false, nil
				}
			}
			return true, nil
		case reflect.Map:
			if reflectutils.GetKind(actual) != reflect.Map {
				return false, fmt.Errorf("invalid actual value, must be a map, found %s", reflectutils.GetKind(actual))
			}
			iter := reflect.ValueOf(expected).MapRange()
			for iter.Next() {
				actualValue := reflect.ValueOf(actual).MapIndex(iter.Key())
				if !actualValue.IsValid() {
					return false, nil
				}
				if inner, err := Match(iter.Value().Interface(), actualValue.Interface()); err != nil {
					return false, err
				} else if !inner {
					return false, nil
				}
			}
			return true, nil
		}
	}
	return Equal(expected, actual)
}
