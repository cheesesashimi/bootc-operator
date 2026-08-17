//go:build tools
// +build tools

// Package tools is a dummy package that is ignored for builds but forces
// `go mod` to retain the code-generation tools as dependencies.
package tools

import (
	_ "k8s.io/code-generator/cmd/applyconfiguration-gen"
	_ "k8s.io/code-generator/cmd/client-gen"
	_ "k8s.io/code-generator/cmd/deepcopy-gen"
	_ "k8s.io/code-generator/cmd/informer-gen"
	_ "k8s.io/code-generator/cmd/lister-gen"
)
