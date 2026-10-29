package run

import (
	"reflect"
	"testing"
)

// clone copies everything a change could edit in place, field by field: a
// field added to Run, or to anything it holds, that clone shares with r is
// caught here, not in a run that remembers less after a change (#266).
func TestCloneSharesNothingAChangeCouldEdit(t *testing.T) {
	var r, want Run
	fill(reflect.ValueOf(&r).Elem())
	fill(reflect.ValueOf(&want).Elem())
	if !reflect.DeepEqual(r, want) {
		t.Fatal("fill isn't the same twice")
	}
	c := r.clone()
	edit(reflect.ValueOf(&c).Elem())
	if reflect.DeepEqual(c, want) {
		t.Fatal("edit changed nothing")
	}
	if !reflect.DeepEqual(r, want) {
		t.Errorf("a change to the clone reached the run:\n got %+v\nwant %+v", r, want)
	}
}

// fill sets every exported field reachable from v, through structs, pointers,
// slices and maps, to a value that isn't its zero: one element per slice and
// map, so there is something to share.
func fill(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString("a")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fill(v.Elem())
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
		fill(v.Index(0))
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		k, e := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		fill(k)
		fill(e)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				fill(v.Field(i))
			}
		}
	}
}

// edit changes, in place, every value v reaches through pointers, slices and
// maps: what a change to a clone that shared any of them would write into the
// original. v's own fields may be replaced; that never reaches the original.
func edit(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "b")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.Pointer:
		if !v.IsNil() {
			edit(v.Elem())
		}
	case reflect.Slice:
		for i := range v.Len() {
			edit(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			edit(e)
			v.SetMapIndex(k, e)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				edit(v.Field(i))
			}
		}
	}
}
