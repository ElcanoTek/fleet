// Copied from github.com/go-chi/chi/v5 v5.3.2, middleware/middleware_test.go
// (the assert helpers only). Copyright (c) 2015-present Peter Kieltyka,
// Google Inc.; see LICENSE. SPDX-License-Identifier: MIT

package httpmw

import (
	"reflect"
	"testing"
)

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("expecting no error")
	}
}

func assertEqual(t *testing.T, a, b interface{}) {
	t.Helper()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("expecting values to be equal but got: '%v' and '%v'", a, b)
	}
}
