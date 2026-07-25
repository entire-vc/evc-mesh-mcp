package mcp

import "testing"

func TestDeliberateCIRedProof(t *testing.T) {
	t.Fatal("intentional failure to verify the go-test CI gate goes red (task acc5d97c AC#2) — reverted after verification")
}
