//go:build !linux

package nginx

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// errShadowNeedsLinux is every shadow test's error where there are no Linux
// mount namespaces. nginx is managed only on Linux; this keeps the package
// building on the macOS dev host.
var errShadowNeedsLinux = errors.New("shadow tests need Linux: they run nginx -t in a private mount namespace")

// noShadow is the ShadowTester of hosts without Linux mount namespaces.
type noShadow struct{}

func (noShadow) Test(context.Context, string, string) (TestOutput, error) {
	return TestOutput{}, errShadowNeedsLinux
}

// newShadowTester is the shadow tester of the operation in dir. On this
// platform every test fails with "shadow tests need Linux".
func newShadowTester(string, helperInput, io.Writer) ShadowTester { return noShadow{} }

// shadowChild is `nexusd nginx-op shadow-test --dir D`, which needs Linux.
func shadowChild(_ string, stderr io.Writer) int {
	fmt.Fprintf(stderr, "nexusd %s shadow-test: %v\n", HelperCommand, errShadowNeedsLinux)
	return 1
}
