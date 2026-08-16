// Command instigator is a network install server for SGI IRIX systems,
// serving untouched CD images over BOOTP, TFTP, and rsh.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "instigator: early development; nothing serves yet")
	os.Exit(1)
}
