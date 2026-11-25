//go:build desktop && !unix && !windows

package main

import "os/exec"

func hideClientWindow(*exec.Cmd) {}
