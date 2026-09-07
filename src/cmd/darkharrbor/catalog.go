package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/darkharrbor/darkharrbor/internal/httpstream/generic"
	"github.com/darkharrbor/darkharrbor/internal/securefile"
)

func runCatalogCommand(args []string) int {
	if len(args) != 1 || args[0] != "import" {
		fmt.Fprintln(os.Stderr, "usage: darkharrbor catalog import < descriptors.json")
		return 2
	}
	const path = "/config/cloud-descriptors.json"
	count, err := importCatalog(os.Stdin, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "catalog import: %v\n", err)
		return 1
	}
	fmt.Fprintf(os.Stdout, "catalog import: installed %d validated descriptors at %s\n", count, path)
	return 0
}

func importCatalog(in io.Reader, path string) (int, error) {
	body, err := io.ReadAll(io.LimitReader(in, generic.MaxDescriptorFileBytes+1))
	if err != nil {
		return 0, fmt.Errorf("read catalog: %w", err)
	}
	descriptors, err := generic.ValidateDescriptors(body)
	if err != nil {
		return 0, err
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || path == string(filepath.Separator) {
		return 0, fmt.Errorf("destination must be a safe absolute file path")
	}
	if err := securefile.PrepareDir(filepath.Dir(path)); err != nil {
		return 0, fmt.Errorf("prepare private catalog directory: %w", err)
	}
	if err := securefile.AtomicWrite(path, body); err != nil {
		return 0, fmt.Errorf("publish private catalog: %w", err)
	}
	return len(descriptors), nil
}
