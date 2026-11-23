//go:build !desktop

package main

import "fmt"

func main() {
	fmt.Println("amux-desktop requires the desktop build tag: go run -tags desktop ./cmd/amux-desktop")
}
