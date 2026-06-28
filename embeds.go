package main

import "embed"

//go:embed internal/init
//go:embed go.mod go.sum
var embedFiles embed.FS
