package main

import (
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Capture the JSON-visible values without serializing unchanged floats or
// strings. Containers are copied so later source updates cannot mutate the
// previous snapshot. time.Time remains a leaf with its standard JSON encoding.
func statusValue(v reflect.Value) any {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	if v.Type() == reflect.TypeFor[time.Time]() {
		return v.Interface()
	}
	switch v.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.PkgPath != "" {
				continue
			}
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			value := v.Field(i)
			if strings.Contains(","+options+",", ",omitempty,") && statusEmpty(value) {
				continue
			}
			if field.Anonymous && name == "" {
				if fields, ok := statusValue(value).(map[string]any); ok {
					for k, value := range fields {
						out[k] = value
					}
				}
				continue
			}
			if name == "" {
				name = field.Name
			}
			out[name] = statusValue(value)
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return nil
		}
		out := map[string]any{}
		iter := v.MapRange()
		for iter.Next() {
			out[iter.Key().String()] = statusValue(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			return nil
		}
		out := make([]any, v.Len())
		for i := range out {
			out[i] = statusValue(v.Index(i))
		}
		return out
	default:
		return v.Interface()
	}
}

func statusEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

// Changes are [JSON-pointer, value] to set, or [JSON-pointer] to delete.
// Source arrays are diffed by index; a length change replaces the array.
// Output windows are a batch of new points and are always replaced as a batch.
func statusDiff(before, after any, path string, changes *[][]any) {
	if reflect.DeepEqual(before, after) {
		return
	}
	oldMap, oldOK := before.(map[string]any)
	newMap, newOK := after.(map[string]any)
	if oldOK && newOK {
		for key := range oldMap {
			if _, ok := newMap[key]; !ok {
				*changes = append(*changes, []any{statusPath(path, key)})
			}
		}
		for key, value := range newMap {
			p := statusPath(path, key)
			if old, ok := oldMap[key]; ok {
				statusDiff(old, value, p, changes)
			} else {
				*changes = append(*changes, []any{p, value})
			}
		}
		return
	}
	oldArray, oldOK := before.([]any)
	newArray, newOK := after.([]any)
	if oldOK && newOK && len(oldArray) == len(newArray) && !strings.HasSuffix(path, "/output") && !strings.HasSuffix(path, "/windows") {
		for i, value := range newArray {
			statusDiff(oldArray[i], value, statusPath(path, strconv.Itoa(i)), changes)
		}
		return
	}
	*changes = append(*changes, []any{path, after})
}

func statusPath(parent, key string) string {
	return parent + "/" + strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}
