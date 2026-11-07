package testsupport

import "testing"

// Expect reports a failed check and lets the test go on; it keeps tests short and flat (gocyclo).
func Expect(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Errorf(format, args...)
	}
}

// Require stops the test when a check fails.
func Require(t *testing.T, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
