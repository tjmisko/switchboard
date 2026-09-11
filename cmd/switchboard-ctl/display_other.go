//go:build !linux

package main

func cmdDisplay(args []string, socket string) {
	fail("display control is currently available on Linux; use list-all/subscribe-all for other consumers")
}
