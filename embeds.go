package main

import "embed"

//go:embed init
//go:embed go.mod go.sum
var embedFiles embed.FS
