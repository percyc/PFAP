package store

import (
	"github.com/pfap/lab/internal/model"
	"reflect"
	"sync"
	"time"
)

var mutableTypes sync.Map // reflect.Type -> bool

// Strings and time.Time are immutable here. In particular, transaction history
// is a large slice of value-only records: copy its backing array once rather
// than recursively allocating/visiting every scalar field on every read.
func mutableType(t reflect.Type) bool {
	if cached, ok := mutableTypes.Load(t); ok {
		return cached.(bool)
	}
	mutable := false
	switch t.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map:
		mutable = true
	case reflect.Array:
		mutable = mutableType(t.Elem())
	case reflect.Struct:
		if t != reflect.TypeOf(time.Time{}) {
			for i := 0; i < t.NumField(); i++ {
				if mutableType(t.Field(i).Type) {
					mutable = true
					break
				}
			}
		}
	}
	mutableTypes.Store(t, mutable)
	return mutable
}

// Only committed, JSON-decoded values enter this cache. Scalars and immutable
// time.Time values may be shared; every mutable container is copied for callers.
func cloneSnapshot(s model.State) model.State {
	return cloneValue(reflect.ValueOf(s)).Interface().(model.State)
}

func cloneValue(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(cloneValue(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(cloneValue(v.Elem()))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		if !mutableType(v.Type().Elem()) {
			reflect.Copy(out, v)
			return out
		}
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneValue(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		it := v.MapRange()
		for it.Next() {
			out.SetMapIndex(it.Key(), cloneValue(it.Value()))
		}
		return out
	case reflect.Array:
		out := reflect.New(v.Type()).Elem()
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(cloneValue(v.Index(i)))
		}
		return out
	case reflect.Struct:
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(v)
		for i := 0; i < v.NumField(); i++ {
			if mutableType(v.Type().Field(i).Type) {
				out.Field(i).Set(cloneValue(v.Field(i)))
			}
		}
		return out
	default:
		return v
	}
}
