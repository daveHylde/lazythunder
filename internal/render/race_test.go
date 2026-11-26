//go:build race

package render

const budgetScale = 10 // the race detector slows rendering ~10x
