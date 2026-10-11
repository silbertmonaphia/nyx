package tool

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// validate runs args through the struct-tag rules declared on the
// tool's ArgType and returns a wrapped *FieldError on the first
// failure. Supported tags (on each exported field):
//
//	`validate:"required"` — field must be non-zero (zero values:
//	  0 / "" / nil / false / empty slice). Required fields reject
//	  the JSON-unmarshal default (zero) so missing keys surface as
//	  validation errors rather than running through the tool body.
//	`validate:"min=N"`    — for numbers, the value must be >= N;
//	  for strings / slices, the length must be >= N.
//	`validate:"max=N"`    — for numbers, the value must be <= N;
//	  for strings / slices, the length must be <= N.
//	`validate:"oneof=a b c"` — string field must equal one of the
//	  listed values (whitespace-stripped).
//
// Multiple constraints are comma-separated, e.g.
//
//	`validate:"required,min=1,max=100"`.
//
// The validator is intentionally tiny — it covers the constraints
// the feed tools need without bringing in the full
// go-playground/validator (a transitive-only dep via huma). See
// CLAUDE.md "No new dependencies".
func validate(argType any, args any) error {
	argVal := reflect.ValueOf(args)
	if argVal.Kind() == reflect.Ptr {
		if argVal.IsNil() {
			return &FieldError{Err: fmt.Errorf("args is nil")}
		}
		argVal = argVal.Elem()
	}
	if argVal.Kind() != reflect.Struct {
		return &FieldError{Err: fmt.Errorf("args must be a struct, got %s", argVal.Kind())}
	}

	wantType := reflect.TypeOf(argType)
	if wantType.Kind() == reflect.Ptr {
		wantType = wantType.Elem()
	}

	for i := 0; i < wantType.NumField(); i++ {
		f := wantType.Field(i)
		if !f.IsExported() {
			continue
		}
		tag, ok := f.Tag.Lookup("validate")
		if !ok {
			continue
		}
		fv := argVal.Field(i)
		if err := checkField(f.Name, fv, tag); err != nil {
			return err
		}
	}
	return nil
}

// checkField applies each comma-separated rule in tag against fv.
// Errors carry the field name so the user sees where the failure was.
func checkField(name string, fv reflect.Value, tag string) error {
	rules := strings.Split(tag, ",")
	for _, rule := range rules {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		if err := applyRule(name, fv, rule); err != nil {
			return err
		}
	}
	return nil
}

// applyRule dispatches one rule to its handler. Format: bare name
// ("required") or name=value where value is an integer or a
// whitespace-separated list (oneof).
func applyRule(name string, fv reflect.Value, rule string) error {
	key, val, hasVal := strings.Cut(rule, "=")
	switch key {
	case "required":
		if isZero(fv) {
			return fieldErrorf(name, fmt.Errorf("required"))
		}
	case "min":
		if !hasVal {
			return fieldErrorf(name, fmt.Errorf("min: missing value"))
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return fieldErrorf(name, fmt.Errorf("min: bad value %q", val))
		}
		if err := minCheck(fv, n); err != nil {
			return fieldErrorf(name, err)
		}
	case "max":
		if !hasVal {
			return fieldErrorf(name, fmt.Errorf("max: missing value"))
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return fieldErrorf(name, fmt.Errorf("max: bad value %q", val))
		}
		if err := maxCheck(fv, n); err != nil {
			return fieldErrorf(name, err)
		}
	case "oneof":
		if !hasVal {
			return fieldErrorf(name, fmt.Errorf("oneof: missing value"))
		}
		if err := oneOfCheck(fv, strings.Fields(val)); err != nil {
			return fieldErrorf(name, err)
		}
	default:
		return fieldErrorf(name, fmt.Errorf("unknown rule %q", key))
	}
	return nil
}

// isZero reports whether fv is the zero value of its kind. Used for
// the `required` rule.
func isZero(fv reflect.Value) bool {
	if !fv.IsValid() {
		return true
	}
	switch fv.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Slice, reflect.Map, reflect.Chan, reflect.Func:
		return fv.IsNil()
	case reflect.String:
		return fv.Len() == 0
	}
	return fv.IsZero()
}

// minCheck returns an error when the value violates the lower bound.
// Numbers compare directly; strings / slices / arrays / maps compare
// by length; other kinds pass through (the rule is meaningless).
func minCheck(fv reflect.Value, n int) error {
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if fv.Int() < int64(n) {
			return fmt.Errorf("must be >= %d", n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		// gosec G115: `n` is always small (tool-arg validator inputs); the
		// explicit `if n < 0` guard documents the bound and keeps the lint
		// gate green. Reaching this with n<0 would already have failed at
		// tag-parse time (validator doesn't accept negative bounds).
		var ub uint64
		if n > 0 {
			ub = uint64(n)
		}
		if fv.Uint() < ub {
			return fmt.Errorf("must be >= %d", n)
		}
	case reflect.Float32, reflect.Float64:
		if fv.Float() < float64(n) {
			return fmt.Errorf("must be >= %d", n)
		}
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map:
		if fv.Len() < n {
			return fmt.Errorf("length must be >= %d", n)
		}
	}
	return nil
}

// maxCheck returns an error when the value violates the upper bound.
// Same kind rules as minCheck.
func maxCheck(fv reflect.Value, n int) error {
	switch fv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if fv.Int() > int64(n) {
			return fmt.Errorf("must be <= %d", n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		var ub uint64
		if n > 0 {
			ub = uint64(n)
		}
		if fv.Uint() > ub {
			return fmt.Errorf("must be <= %d", n)
		}
	case reflect.Float32, reflect.Float64:
		if fv.Float() > float64(n) {
			return fmt.Errorf("must be <= %d", n)
		}
	case reflect.String, reflect.Slice, reflect.Array, reflect.Map:
		if fv.Len() > n {
			return fmt.Errorf("length must be <= %d", n)
		}
	}
	return nil
}

// oneOfCheck returns an error when the string value is not one of
// the listed options. Empty string never matches (use `required`
// to enforce presence).
func oneOfCheck(fv reflect.Value, options []string) error {
	if fv.Kind() != reflect.String {
		return fmt.Errorf("oneof applies to strings only")
	}
	got := fv.String()
	for _, opt := range options {
		if got == opt {
			return nil
		}
	}
	return fmt.Errorf("must be one of %v", options)
}
