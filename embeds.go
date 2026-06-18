package main

import "embed"

//go:embed init
//go:embed go.mod go.sum
//go:embed kernel
var embedFiles embed.FS
