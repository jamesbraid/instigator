// Command instigator is a network install server for SGI IRIX systems,
// serving untouched CD images over BOOTP, TFTP, and rsh.
package main

import (
	"fmt"
	"os"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  instigator ls <image> [path]   list an SGI CD image (volume header + EFS)`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "ls":
		if len(os.Args) < 3 || len(os.Args) > 4 {
			usage()
		}
		path := "/"
		if len(os.Args) == 4 {
			path = os.Args[3]
		}
		if err := runLs(os.Stdout, os.Args[2], path); err != nil {
			fmt.Fprintln(os.Stderr, "instigator:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}
