package config

import (
	"reflect"
	"testing"
)

// Every exported (authored) field survives the copy for writing; a Config
// field added later is carried without being listed anywhere.
func TestExportedCopyCarriesEveryExportedField(t *testing.T) {
	src := &Config{}
	v := reflect.ValueOf(src).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if !f.IsExported() {
			continue
		}
		switch f.Type.Kind() {
		case reflect.Map:
			v.Field(i).Set(reflect.MakeMap(f.Type))
		case reflect.Slice:
			v.Field(i).Set(reflect.MakeSlice(f.Type, 1, 1))
		case reflect.Pointer:
			v.Field(i).Set(reflect.New(f.Type.Elem()))
		case reflect.String:
			v.Field(i).SetString("x")
		case reflect.Float64, reflect.Float32:
			v.Field(i).SetFloat(1)
		case reflect.Int, reflect.Int64, reflect.Int32:
			v.Field(i).SetInt(1)
		case reflect.Bool:
			v.Field(i).SetBool(true)
		}
	}
	out := exportedCopy(src)
	ov := reflect.ValueOf(out).Elem()
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.IsExported() && !reflect.DeepEqual(v.Field(i).Interface(), ov.Field(i).Interface()) {
			t.Errorf("field %s not carried", f.Name)
		}
	}
}
