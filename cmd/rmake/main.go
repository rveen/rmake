// Command rmake is a content-hash based build tool that can import Makefiles.
// See README.md for the specification.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "rmake: not implemented yet (see README.md, milestone M1)")
	os.Exit(2)
}
