package tool

import (
	"errors"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validateFixtureArgs exercises the validator's field-selection rules
// directly: two tagged fields with passable values, one untagged
// field (skipped), one unexported tagged field (skipped — IsExported
// gates it, so its zero value must NOT trip `required`).
//
// The production arg structs all pass through Dispatch, which covers
// the happy path; these direct calls reach the branches Dispatch
// can't (nil args, non-struct args, pointer argType, malformed tags).
type validateFixtureArgs struct {
	Title  string `validate:"required,min=1,max=5"`
	Order  string `validate:"oneof=asc desc"`
	Bare   string // no tag → skipped
	hidden string `validate:"required"` //nolint:unused // exercised via reflect
}

func TestValidate_NilPointerArgs(t *testing.T) {
	var p *validateFixtureArgs
	err := validate(validateFixtureArgs{}, p)
	require.Error(t, err)

	var fe *FieldError
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, "args is nil", fe.Err.Error())
}

func TestValidate_NonStructArgs(t *testing.T) {
	err := validate(struct{}{}, 42)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "args must be a struct")
}

// TestValidate_PointerArgType confirms an ArgType passed as a pointer
// (the shape huma-style registration would produce) is unwrapped to
// its element type before field iteration.
func TestValidate_PointerArgType(t *testing.T) {
	err := validate(&validateFixtureArgs{}, validateFixtureArgs{Title: "ok", Order: "asc"})
	assert.NoError(t, err)
}

// TestValidate_PointerArgs covers the args-side pointer unwrap: a
// non-nil pointer is dereferenced before field iteration, so both the
// passing and failing cases are reachable through the pointer form.
func TestValidate_PointerArgs(t *testing.T) {
	assert.NoError(t, validate(validateFixtureArgs{}, &validateFixtureArgs{Title: "ok", Order: "asc"}))

	err := validate(validateFixtureArgs{}, &validateFixtureArgs{Order: "asc"}) // Title empty
	require.Error(t, err)
	var fe *FieldError
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, "Title", fe.Field)
}

// TestValidate_SkipsUntaggedAndUnexported — Bare carries no tag and
// hidden is unexported, so both are skipped even though `hidden` is
// zero and would fail its `required` rule if it were consulted.
func TestValidate_SkipsUntaggedAndUnexported(t *testing.T) {
	err := validate(validateFixtureArgs{}, validateFixtureArgs{Title: "ok", Order: "asc"})
	assert.NoError(t, err)
}

// TestValidate_ReportsFirstFailingField pins multi-field ordering:
// Title (field 0) fails before Order is ever checked.
func TestValidate_ReportsFirstFailingField(t *testing.T) {
	err := validate(validateFixtureArgs{}, validateFixtureArgs{Title: "", Order: "sideways"})
	require.Error(t, err)
	var fe *FieldError
	require.True(t, errors.As(err, &fe))
	assert.Equal(t, "Title", fe.Field)
}

// TestApplyRule_MalformedRules pins the parse errors for every rule:
// missing `=value`, non-integer bounds, and unknown rule names all
// surface as FieldErrors naming the field (never a panic).
func TestApplyRule_MalformedRules(t *testing.T) {
	fv := reflect.ValueOf(5)
	for _, c := range []struct {
		name, rule, want string
	}{
		{"min missing value", "min", "min: missing value"},
		{"min bad value", "min=abc", `min: bad value "abc"`},
		{"max missing value", "max", "max: missing value"},
		{"max bad value", "max=abc", `max: bad value "abc"`},
		{"oneof missing value", "oneof", "oneof: missing value"},
		{"unknown rule", "frobnicate=1", `unknown rule "frobnicate"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := applyRule("F", fv, c.rule)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.want)

			var fe *FieldError
			require.True(t, errors.As(err, &fe))
			assert.Equal(t, "F", fe.Field)
		})
	}
}

// TestApplyRule_PassingRules — the no-error side of applyRule, so a
// regression that turns every rule into a failure is caught here
// rather than only via the fancier Dispatch tests.
func TestApplyRule_PassingRules(t *testing.T) {
	assert.NoError(t, applyRule("F", reflect.ValueOf(3), "min=1"))
	assert.NoError(t, applyRule("F", reflect.ValueOf(3), "max=5"))
	assert.NoError(t, applyRule("F", reflect.ValueOf("asc"), "oneof=asc desc"))
	assert.NoError(t, applyRule("F", reflect.ValueOf(1), "required"))
}

// TestCheckField_SkipsEmptyRules — whitespace-only segments between
// commas are legal and skipped (tags like " , min=1 , " must parse).
func TestCheckField_SkipsEmptyRules(t *testing.T) {
	assert.NoError(t, checkField("F", reflect.ValueOf(1), " , min=1 , "))
	assert.NoError(t, checkField("F", reflect.ValueOf(1), " "))
	assert.Error(t, checkField("F", reflect.ValueOf(0), "min=1"))
}

// TestIsZero_Kinds walks every kind isZero special-cases: nil-able
// kinds (ptr / interface / slice / map / chan / func) report via
// IsNil, strings via Len, everything else via reflect.IsZero.
func TestIsZero_Kinds(t *testing.T) {
	assert.True(t, isZero(reflect.Value{}), "invalid Value must be treated as zero")

	x := 1
	assert.True(t, isZero(reflect.ValueOf((*int)(nil))))
	assert.False(t, isZero(reflect.ValueOf(&x)))

	var iface any
	assert.True(t, isZero(reflect.ValueOf(&iface).Elem()))
	iface = 1
	assert.False(t, isZero(reflect.ValueOf(&iface).Elem()))

	var nilSlice []int
	assert.True(t, isZero(reflect.ValueOf(nilSlice)))
	assert.False(t, isZero(reflect.ValueOf([]int{})))

	var nilMap map[string]int
	assert.True(t, isZero(reflect.ValueOf(nilMap)))
	assert.False(t, isZero(reflect.ValueOf(map[string]int{"a": 1})))

	assert.True(t, isZero(reflect.ValueOf((chan int)(nil))))
	assert.False(t, isZero(reflect.ValueOf(make(chan int))))

	var fn func()
	assert.True(t, isZero(reflect.ValueOf(fn)))
	assert.False(t, isZero(reflect.ValueOf(func() {})))

	assert.True(t, isZero(reflect.ValueOf("")))
	assert.False(t, isZero(reflect.ValueOf("x")))
	assert.True(t, isZero(reflect.ValueOf(0)))
	assert.False(t, isZero(reflect.ValueOf(1)))
	assert.True(t, isZero(reflect.ValueOf(false)))
	assert.False(t, isZero(reflect.ValueOf(true)))
}

// TestMinCheck_Kinds / TestMaxCheck_Kinds drive both bound functions
// across every numeric kind plus length-based kinds, in both the
// passing and violating direction. Kinds with no meaningful bound
// (bool) pass through untouched.
func TestMinCheck_Kinds(t *testing.T) {
	assert.NoError(t, minCheck(reflect.ValueOf(5), 5))
	assert.EqualError(t, minCheck(reflect.ValueOf(4), 5), "must be >= 5")

	assert.NoError(t, minCheck(reflect.ValueOf(uint(4)), 4))
	assert.EqualError(t, minCheck(reflect.ValueOf(uint(3)), 4), "must be >= 4")
	// A negative bound is the case the `if n > 0` guard exists for
	// (gosec G115): without it, n would wrap to a huge uint64 bound
	// and this would wrongly fail.
	assert.NoError(t, minCheck(reflect.ValueOf(uint(5)), -1))

	assert.NoError(t, minCheck(reflect.ValueOf(2.5), 2))
	assert.EqualError(t, minCheck(reflect.ValueOf(1.5), 2), "must be >= 2")

	assert.NoError(t, minCheck(reflect.ValueOf("abc"), 3))
	assert.EqualError(t, minCheck(reflect.ValueOf("ab"), 3), "length must be >= 3")
	assert.NoError(t, minCheck(reflect.ValueOf([]int{1, 2}), 2))
	assert.EqualError(t, minCheck(reflect.ValueOf([]int{1}), 2), "length must be >= 2")
	assert.NoError(t, minCheck(reflect.ValueOf([2]int{1, 2}), 2))
	assert.EqualError(t, minCheck(reflect.ValueOf([1]int{1}), 2), "length must be >= 2")
	assert.NoError(t, minCheck(reflect.ValueOf(map[string]int{"a": 1}), 1))
	assert.EqualError(t, minCheck(reflect.ValueOf(map[string]int{}), 1), "length must be >= 1")

	assert.NoError(t, minCheck(reflect.ValueOf(true), 99), "kinds without a bound pass through")
}

func TestMaxCheck_Kinds(t *testing.T) {
	assert.NoError(t, maxCheck(reflect.ValueOf(5), 5))
	assert.EqualError(t, maxCheck(reflect.ValueOf(6), 5), "must be <= 5")

	assert.NoError(t, maxCheck(reflect.ValueOf(uint(4)), 4))
	assert.EqualError(t, maxCheck(reflect.ValueOf(uint(5)), 4), "must be <= 4")
	assert.NoError(t, maxCheck(reflect.ValueOf(uint(0)), 0))
	// Same guard as minCheck: a negative bound clamps to 0 rather
	// than wrapping, so any non-zero unsigned value trips the bound.
	assert.EqualError(t, maxCheck(reflect.ValueOf(uint(5)), -1), "must be <= -1")

	assert.NoError(t, maxCheck(reflect.ValueOf(2.5), 3))
	assert.EqualError(t, maxCheck(reflect.ValueOf(3.5), 3), "must be <= 3")

	assert.NoError(t, maxCheck(reflect.ValueOf("abc"), 3))
	assert.EqualError(t, maxCheck(reflect.ValueOf("abcd"), 3), "length must be <= 3")
	assert.NoError(t, maxCheck(reflect.ValueOf([]int{1, 2}), 2))
	assert.EqualError(t, maxCheck(reflect.ValueOf([]int{1, 2, 3}), 2), "length must be <= 2")
	assert.NoError(t, maxCheck(reflect.ValueOf([2]int{1, 2}), 2))
	assert.EqualError(t, maxCheck(reflect.ValueOf([3]int{1, 2, 3}), 2), "length must be <= 2")
	assert.NoError(t, maxCheck(reflect.ValueOf(map[string]int{"a": 1}), 1))
	assert.EqualError(t, maxCheck(reflect.ValueOf(map[string]int{"a": 1, "b": 2}), 1), "length must be <= 1")

	assert.NoError(t, maxCheck(reflect.ValueOf(true), 0), "kinds without a bound pass through")
}

// TestOneOfCheck — match, mismatch (case-sensitive), and the
// non-string rejection.
func TestOneOfCheck(t *testing.T) {
	assert.NoError(t, oneOfCheck(reflect.ValueOf("asc"), []string{"asc", "desc"}))
	assert.EqualError(t, oneOfCheck(reflect.ValueOf("ASC"), []string{"asc"}), "must be one of [asc]")
	assert.EqualError(t, oneOfCheck(reflect.ValueOf(3), []string{"3"}),
		"oneof applies to strings only")
}
