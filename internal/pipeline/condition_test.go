package pipeline

import (
	"strings"
	"testing"
)

func TestEvalCondition(t *testing.T) {
	env := map[string]string{
		"BRANCH":  "main",
		"CI":      "true",
		"EMPTY":   "",
		"ZERO":    "0",
		"FALSE":   "false",
		"TAG":     "v1.2.3",
		"NAME":    "forge",
		"NUMERIC": "42",
	}

	tests := []struct {
		expr string
		want bool
	}{
		// Empty expression means "no condition".
		{"", true},
		{"   ", true},

		// Equality against quoted and bare literals.
		{`$BRANCH == "main"`, true},
		{`$BRANCH == main`, true},
		{`$BRANCH == "develop"`, false},
		{`$BRANCH != "develop"`, true},
		{`$BRANCH != "main"`, false},
		{`${BRANCH} == "main"`, true},
		{`'main' == $BRANCH`, true},

		// Truthiness of bare values.
		{`$CI`, true},
		{`$EMPTY`, false},
		{`$ZERO`, false},
		{`$FALSE`, false},
		{`$MISSING`, false},
		{`$NAME`, true},

		// Negation.
		{`!$EMPTY`, true},
		{`!$CI`, false},
		{`!($BRANCH == "main")`, false},
		{`!!$CI`, true},

		// Boolean combinators.
		{`$CI && $BRANCH == "main"`, true},
		{`$CI && $BRANCH == "develop"`, false},
		{`$EMPTY || $CI`, true},
		{`$EMPTY || $ZERO`, false},
		{`$BRANCH == "main" || $BRANCH == "develop"`, true},
		{`($EMPTY || $CI) && $NAME == "forge"`, true},
		{`$EMPTY || ($CI && $NAME == "nope")`, false},

		// Precedence: && binds tighter than ||.
		{`$EMPTY && $CI || $CI`, true},
		{`$CI || $EMPTY && $EMPTY`, true},

		// Comparing a missing variable to the empty string is the idiomatic
		// "is this set" check.
		{`$MISSING == ""`, true},
		{`$CI != ""`, true},

		// Regex matching.
		{`$TAG =~ "^v[0-9]+"`, true},
		{`$TAG =~ "^release"`, false},
		{`$TAG !~ "^release"`, true},
		{`$NUMERIC =~ "^[0-9]+$"`, true},
	}

	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			got, err := EvalCondition(tc.expr, env)
			if err != nil {
				t.Fatalf("EvalCondition(%q) error = %v", tc.expr, err)
			}
			if got != tc.want {
				t.Errorf("EvalCondition(%q) = %v, want %v", tc.expr, got, tc.want)
			}
		})
	}
}

func TestEvalConditionNilEnv(t *testing.T) {
	got, err := EvalCondition(`$ANYTHING == ""`, nil)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if !got {
		t.Error("an unset variable in a nil env should compare equal to the empty string")
	}
}

func TestParseConditionErrors(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{`$BRANCH ==`, "expected a value"},
		{`== "main"`, "expected a value"},
		{`("main"`, "expected )"},
		{`"unterminated`, "unterminated string"},
		{`$`, "expected a variable name"},
		{`${BRANCH`, "unterminated ${...}"},
		{`$A == "x" $B`, "unexpected"},
		{`$TAG =~ "["`, "invalid regular expression"},
		{`$TAG =~ $OTHER`, "must be a literal pattern"},
	}
	for _, tc := range tests {
		t.Run(tc.expr, func(t *testing.T) {
			_, err := ParseCondition(tc.expr)
			if err == nil {
				t.Fatalf("ParseCondition(%q) succeeded, want an error", tc.expr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestConditionShortCircuits(t *testing.T) {
	// The right side of these expressions would be a regex error if compiled at
	// eval time; compilation happens at parse time, so this just confirms that
	// short-circuiting produces the documented result.
	env := map[string]string{"A": "", "B": "set"}
	got, err := EvalCondition(`$A && $UNDEFINED == "x"`, env)
	if err != nil || got {
		t.Errorf("&& should short-circuit to false, got %v (err %v)", got, err)
	}
	got, err = EvalCondition(`$B || $UNDEFINED == "x"`, env)
	if err != nil || !got {
		t.Errorf("|| should short-circuit to true, got %v (err %v)", got, err)
	}
}

func TestConditionStringRoundTrip(t *testing.T) {
	expr := `$BRANCH == "main" && $CI`
	cond, err := ParseCondition(expr)
	if err != nil {
		t.Fatalf("ParseCondition() error = %v", err)
	}
	if cond.String() != expr {
		t.Errorf("String() = %q, want %q", cond.String(), expr)
	}
}

func TestTruthy(t *testing.T) {
	falsey := []string{"", " ", "0", "false", "FALSE", "False", "no", "off", "null", "nil"}
	for _, v := range falsey {
		if truthy(v) {
			t.Errorf("truthy(%q) = true, want false", v)
		}
	}
	for _, v := range []string{"1", "true", "yes", "on", "anything", "00", "-1"} {
		if !truthy(v) {
			t.Errorf("truthy(%q) = false, want true", v)
		}
	}
}
