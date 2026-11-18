//go:build !unix

package main

func termWidth(fd uintptr) int { return 0 }
