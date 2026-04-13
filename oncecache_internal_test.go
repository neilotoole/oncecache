package oncecache

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestApplyOpts_UnknownPanics exercises the defensive panic in applyOpts
// when an Opt is none of the recognized internal kinds. Because the [Opt]
// interface is closed (the marker method is unexported), this branch is
// unreachable from outside the package; we test it via an internal stub.
func TestApplyOpts_UnknownPanics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic from applyOpts on unknown Opt type")
		}
		if s, ok := r.(string); !ok || !strings.Contains(s, "Invalid Opt type") {
			t.Fatalf("unexpected panic value: %v", r)
		}
	}()
	c := New[int, int](func(_ context.Context, k int) (int, error) { return k, nil })
	c.applyOpts([]Opt{badOpt{}})
}

// TestRandomName_Format verifies the documented "cache-XXXXXXXX" shape:
// 8 lowercase hex digits.
func TestRandomName_Format(t *testing.T) {
	t.Parallel()
	const prefix = "cache-"
	for i := 0; i < 20; i++ {
		name := randomName()
		if !strings.HasPrefix(name, prefix) {
			t.Fatalf("missing prefix: %q", name)
		}
		hex := name[len(prefix):]
		if len(hex) != 8 {
			t.Fatalf("expected 8 hex digits, got %d in %q", len(hex), name)
		}
		for _, r := range hex {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Fatalf("non-hex character %q in %q", r, name)
			}
		}
	}
}

// TestIsNil verifies the kind-aware nil check across nilable and
// non-nilable kinds.
func TestIsNil(t *testing.T) {
	t.Parallel()

	if !isNil(nil) {
		t.Error("plain nil")
	}

	var p *int
	if !isNil(p) {
		t.Error("nil *int")
	}
	x := 0
	if isNil(&p) || isNil(&x) {
		t.Error("non-nil pointers")
	}

	var s []int
	if !isNil(s) {
		t.Error("nil []int")
	}
	if isNil([]int{}) {
		t.Error("empty (non-nil) slice")
	}

	var m map[string]int
	if !isNil(m) {
		t.Error("nil map")
	}
	if isNil(map[string]int{}) {
		t.Error("empty (non-nil) map")
	}

	var fn func()
	if !isNil(fn) {
		t.Error("nil func")
	}
	if isNil(func() {}) {
		t.Error("non-nil func")
	}

	var ch chan int
	if !isNil(ch) {
		t.Error("nil chan")
	}

	// Non-nilable kinds: should always return false (and must not panic).
	if isNil(0) {
		t.Error("int 0 must not be nil")
	}
	if isNil(false) {
		t.Error("bool false must not be nil")
	}
	if isNil("") {
		t.Error("empty string must not be nil")
	}
	if isNil(struct{}{}) {
		t.Error("empty struct must not be nil")
	}
	type s2 struct{ x int }
	if isNil(s2{}) {
		t.Error("zero-value struct must not be nil")
	}
}

// TestUniq covers the small uniq helper across degenerate and typical
// inputs.
func TestUniq(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want []int
	}{
		{nil, []int{}},
		{[]int{}, []int{}},
		{[]int{1}, []int{1}},
		{[]int{1, 1, 1}, []int{1}},
		{[]int{1, 2, 3}, []int{1, 2, 3}},
		{[]int{3, 1, 2, 1, 3}, []int{3, 1, 2}}, // first-occurrence order preserved
	}
	for _, tc := range cases {
		got := uniq(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("uniq(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestFillPanic_Error_Format verifies the format of the wrapped error
// string and its Unwrap chain.
func TestFillPanic_Error_Format(t *testing.T) {
	t.Parallel()
	p := &fillPanic{recovered: "boom"}
	if !strings.HasSuffix(p.Error(), ": boom") {
		t.Errorf("error format: %q", p.Error())
	}
	if p.Unwrap() != ErrPanic {
		t.Errorf("Unwrap should yield ErrPanic, got %v", p.Unwrap())
	}
}

// badOpt satisfies Opt (the marker method is unexported, but we can
// implement it in this internal test package) without implementing any of
// the recognized internal kinds. It exists solely to drive the panic
// branch in applyOpts.
type badOpt struct{}

func (badOpt) optioner() {}
