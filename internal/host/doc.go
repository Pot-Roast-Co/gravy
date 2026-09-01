// Package host is the only package permitted to execute commands.
//
// Rule 1 of ARCHITECTURE.md 1.1: no package outside this one may import os/exec or
// construct worktree paths directly. That restriction is what makes remote hosts a later
// addition rather than a rewrite.
package host
